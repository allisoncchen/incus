package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v2"

	incus "github.com/lxc/incus/v6/client"
	cli "github.com/lxc/incus/v6/internal/cmd"
	"github.com/lxc/incus/v6/internal/linux"
	"github.com/lxc/incus/v6/internal/version"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/archive"
	"github.com/lxc/incus/v6/shared/osarch"
	"github.com/lxc/incus/v6/shared/revert"
	localtls "github.com/lxc/incus/v6/shared/tls"
	"github.com/lxc/incus/v6/shared/units"
	"github.com/lxc/incus/v6/shared/util"
)

type cmdMigrate struct {
	global *cmdGlobal

	flagRsyncArgs string
}

func (c *cmdMigrate) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "incus-migrate"
	cmd.Short = "Physical to instance migration tool"
	cmd.Long = `Description:
  Physical to instance migration tool

  This tool lets you turn any Linux filesystem (including your current one)
  into an instance on a remote host.

  It will setup a clean mount tree made of the root filesystem and any
  additional mount you list, then transfer this through the migration
  API to create a new instance from it.

  The same set of options as ` + "`incus launch`" + ` are also supported.
`
	cmd.RunE = c.run
	cmd.Flags().StringVar(&c.flagRsyncArgs, "rsync-args", "", "Extra arguments to pass to rsync (for file transfers)"+"``")

	return cmd
}

type cmdMigrateData struct {
	SourcePath       string
	SourceFormat     string
	Mounts           []string
	InstanceArgs     api.InstancesPost
	CustomVolumeArgs api.StorageVolumesPost
	Pool             string
	Project          string
}

func (c *cmdMigrateData) renderInstance() string {
	data := struct {
		Name         string            `yaml:"Name"`
		Project      string            `yaml:"Project"`
		Type         api.InstanceType  `yaml:"Type"`
		Source       string            `yaml:"Source"`
		SourceFormat string            `yaml:"Source format,omitempty"`
		Mounts       []string          `yaml:"Mounts,omitempty"`
		Profiles     []string          `yaml:"Profiles,omitempty"`
		StoragePool  string            `yaml:"Storage pool,omitempty"`
		StorageSize  string            `yaml:"Storage pool size,omitempty"`
		Network      string            `yaml:"Network name,omitempty"`
		Config       map[string]string `yaml:"Config,omitempty"`
	}{
		c.InstanceArgs.Name,
		c.Project,
		c.InstanceArgs.Type,
		c.SourcePath,
		c.SourceFormat,
		c.Mounts,
		c.InstanceArgs.Profiles,
		"",
		"",
		"",
		c.InstanceArgs.Config,
	}

	disk, ok := c.InstanceArgs.Devices["root"]
	if ok {
		data.StoragePool = disk["pool"]

		size, ok := disk["size"]
		if ok {
			data.StorageSize = size
		}
	}

	network, ok := c.InstanceArgs.Devices["eth0"]
	if ok {
		data.Network = network["parent"]
	}

	out, err := yaml.Marshal(&data)
	if err != nil {
		return ""
	}

	return string(out)
}

func (c *cmdMigrateData) renderCustomVolume() string {
	data := struct {
		Name         string `yaml:"Name"`
		Project      string `yaml:"Project"`
		Type         string `yaml:"Type"`
		Source       string `yaml:"Source"`
		SourceFormat string `yaml:"Source format,omitempty"`
	}{
		c.CustomVolumeArgs.Name,
		c.Project,
		c.CustomVolumeArgs.ContentType,
		c.SourcePath,
		c.SourceFormat,
	}

	out, err := yaml.Marshal(&data)
	if err != nil {
		return ""
	}

	return string(out)
}

func (c *cmdMigrate) askServer() (incus.InstanceServer, string, error) {
	// Detect local server.
	local, err := c.connectLocal()
	if err == nil {
		useLocal, err := c.global.asker.AskBool("The local Incus server is the target [default=yes]: ", "yes")
		if err != nil {
			return nil, "", err
		}

		if useLocal {
			return local, "", nil
		}
	}

	// Server address
	serverURL, err := c.global.asker.AskString("Please provide Incus server URL: ", "", nil)
	if err != nil {
		return nil, "", err
	}

	serverURL, err = parseURL(serverURL)
	if err != nil {
		return nil, "", err
	}

	args := incus.ConnectionArgs{
		UserAgent: fmt.Sprintf("LXC-MIGRATE %s", version.Version),
	}

	// Attempt to connect
	server, err := incus.ConnectIncus(serverURL, &args)
	if err != nil {
		// Failed to connect using the system CA, so retrieve the remote certificate.
		certificate, err := localtls.GetRemoteCertificate(serverURL, args.UserAgent)
		if err != nil {
			return nil, "", fmt.Errorf("Failed to get remote certificate: %w", err)
		}

		digest := localtls.CertFingerprint(certificate)

		fmt.Println("Certificate fingerprint:", digest)
		fmt.Print("ok (y/n)? ")

		buf := bufio.NewReader(os.Stdin)
		line, _, err := buf.ReadLine()
		if err != nil {
			return nil, "", err
		}

		if len(line) < 1 || line[0] != 'y' && line[0] != 'Y' {
			return nil, "", fmt.Errorf("Server certificate rejected by user")
		}

		args.InsecureSkipVerify = true
		server, err = incus.ConnectIncus(serverURL, &args)
		if err != nil {
			return nil, "", fmt.Errorf("Failed to connect to server: %w", err)
		}
	}

	apiServer, _, err := server.GetServer()
	if err != nil {
		return nil, "", fmt.Errorf("Failed to get server: %w", err)
	}

	fmt.Println("")

	type AuthMethod int

	const (
		authMethodTLSCertificate AuthMethod = iota
		authMethodTLSTemporaryCertificate
		authMethodTLSCertificateToken
	)

	// TLS is always available
	var availableAuthMethods []AuthMethod
	var authMethod AuthMethod

	i := 1

	if slices.Contains(apiServer.AuthMethods, api.AuthenticationMethodTLS) {
		fmt.Printf("%d) Use a certificate token\n", i)
		availableAuthMethods = append(availableAuthMethods, authMethodTLSCertificateToken)
		i++
		fmt.Printf("%d) Use an existing TLS authentication certificate\n", i)
		availableAuthMethods = append(availableAuthMethods, authMethodTLSCertificate)
		i++
		fmt.Printf("%d) Generate a temporary TLS authentication certificate\n", i)
		availableAuthMethods = append(availableAuthMethods, authMethodTLSTemporaryCertificate)
	}

	if len(apiServer.AuthMethods) > 1 || slices.Contains(apiServer.AuthMethods, api.AuthenticationMethodTLS) {
		authMethodInt, err := c.global.asker.AskInt("Please pick an authentication mechanism above: ", 1, int64(i), "", nil)
		if err != nil {
			return nil, "", err
		}

		authMethod = availableAuthMethods[authMethodInt-1]
	}

	var certPath string
	var keyPath string
	var token string

	switch authMethod {
	case authMethodTLSCertificate:
		certPath, err = c.global.asker.AskString("Please provide the certificate path: ", "", func(path string) error {
			if !util.PathExists(path) {
				return errors.New("File does not exist")
			}

			return nil
		})
		if err != nil {
			return nil, "", err
		}

		keyPath, err = c.global.asker.AskString("Please provide the keyfile path: ", "", func(path string) error {
			if !util.PathExists(path) {
				return errors.New("File does not exist")
			}

			return nil
		})
		if err != nil {
			return nil, "", err
		}

	case authMethodTLSCertificateToken:
		token, err = c.global.asker.AskString("Please provide the certificate token: ", "", func(token string) error {
			_, err := localtls.CertificateTokenDecode(token)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return nil, "", err
		}

	case authMethodTLSTemporaryCertificate:
		// Intentionally ignored
	}

	var authType string

	switch authMethod {
	case authMethodTLSCertificate, authMethodTLSTemporaryCertificate, authMethodTLSCertificateToken:
		authType = api.AuthenticationMethodTLS
	}

	return c.connectTarget(serverURL, certPath, keyPath, authType, token)
}

func (c *cmdMigrate) gatherInstanceInfo(server incus.InstanceServer, migrationType MigrationType) (cmdMigrateData, error) {
	var err error

	config := cmdMigrateData{}

	config.InstanceArgs = api.InstancesPost{
		Source: api.InstanceSource{
			Type: "migration",
			Mode: "push",
		},
	}

	config.InstanceArgs.Config = map[string]string{}
	config.InstanceArgs.Devices = map[string]map[string]string{}

	if migrationType == MigrationTypeVM {
		config.InstanceArgs.Type = api.InstanceTypeVM
	} else {
		config.InstanceArgs.Type = api.InstanceTypeContainer
	}

	// Project
	err = c.askProject(server, &config)
	if err != nil {
		return cmdMigrateData{}, err
	}

	if config.Project != "" {
		server = server.UseProject(config.Project)
	}

	// Instance name
	instanceNames, err := server.GetInstanceNames(api.InstanceTypeAny)
	if err != nil {
		return cmdMigrateData{}, err
	}

	for {
		instanceName, err := c.global.asker.AskString("Name of the new instance: ", "", nil)
		if err != nil {
			return cmdMigrateData{}, err
		}

		if slices.Contains(instanceNames, instanceName) {
			fmt.Printf("Instance %q already exists\n", instanceName)
			continue
		}

		config.InstanceArgs.Name = instanceName
		break
	}

	// Provide source path
	err = c.askSourcePath(&config, migrationType)
	if err != nil {
		return cmdMigrateData{}, err
	}

	if config.InstanceArgs.Type == api.InstanceTypeVM {
		architectureName, _ := osarch.ArchitectureGetLocal()

		if slices.Contains([]string{"x86_64", "aarch64"}, architectureName) {
			hasUEFI, err := c.global.asker.AskBool("Does the VM support UEFI booting? [default=yes]: ", "yes")
			if err != nil {
				return cmdMigrateData{}, err
			}

			if hasUEFI {
				hasSecureBoot, err := c.global.asker.AskBool("Does the VM support UEFI Secure Boot? [default=yes]: ", "yes")
				if err != nil {
					return cmdMigrateData{}, err
				}

				if !hasSecureBoot {
					config.InstanceArgs.Config["security.secureboot"] = "false"
				}
			} else {
				config.InstanceArgs.Config["security.csm"] = "true"
				config.InstanceArgs.Config["security.secureboot"] = "false"
			}
		}
	}

	var mounts []string

	// Additional mounts for containers
	if config.InstanceArgs.Type == api.InstanceTypeContainer {
		addMounts, err := c.global.asker.AskBool("Do you want to add additional filesystem mounts? [default=no]: ", "no")
		if err != nil {
			return cmdMigrateData{}, err
		}

		if addMounts {
			for {
				path, err := c.global.asker.AskString("Please provide a path the filesystem mount path [empty value to continue]: ", "", func(s string) error {
					if s != "" {
						if util.PathExists(s) {
							return nil
						}

						return errors.New("Path does not exist")
					}

					return nil
				})
				if err != nil {
					return cmdMigrateData{}, err
				}

				if path == "" {
					break
				}

				mounts = append(mounts, path)
			}

			config.Mounts = append(config.Mounts, mounts...)
		}
	}

	for {
		fmt.Println("\nInstance to be created:")

		scanner := bufio.NewScanner(strings.NewReader(config.renderInstance()))
		for scanner.Scan() {
			fmt.Printf("  %s\n", scanner.Text())
		}

		fmt.Print(`
Additional overrides can be applied at this stage:
1) Begin the migration with the above configuration
2) Override profile list
3) Set additional configuration options
4) Change instance storage pool or volume size
5) Change instance network

`)

		choice, err := c.global.asker.AskInt("Please pick one of the options above [default=1]: ", 1, 5, "1", nil)
		if err != nil {
			return cmdMigrateData{}, err
		}

		switch choice {
		case 1:
			return config, nil
		case 2:
			err = c.askProfiles(server, &config)
		case 3:
			err = c.askConfig(&config)
		case 4:
			err = c.askStorage(server, &config)
		case 5:
			err = c.askNetwork(server, &config)
		}

		if err != nil {
			fmt.Println(err)
		}
	}
}

func (c *cmdMigrate) gatherCustomVolumeInfo(server incus.InstanceServer, migrationType MigrationType) (cmdMigrateData, error) {
	var err error

	config := cmdMigrateData{}

	config.CustomVolumeArgs = api.StorageVolumesPost{
		Type: "custom",
		Source: api.StorageVolumeSource{
			Type: "migration",
			Mode: "push",
		},
	}

	if migrationType == MigrationTypeVolumeFilesystem {
		config.CustomVolumeArgs.ContentType = "filesystem"
	} else {
		config.CustomVolumeArgs.ContentType = "block"
	}

	// Project
	err = c.askProject(server, &config)
	if err != nil {
		return cmdMigrateData{}, err
	}

	if config.Project != "" {
		server = server.UseProject(config.Project)
	}

	// Pool
	pools, err := server.GetStoragePools()
	if err != nil {
		return cmdMigrateData{}, err
	}

	poolNames := []string{}
	for _, p := range pools {
		poolNames = append(poolNames, p.Name)
	}

	for {
		poolName, err := c.global.asker.AskString("Name of the pool: ", "", nil)
		if err != nil {
			return cmdMigrateData{}, err
		}

		if !slices.Contains(poolNames, poolName) {
			fmt.Printf("Pool %q doesn't exists\n", poolName)
			continue
		}

		config.Pool = poolName
		break
	}

	// Custom volume name
	volumes, err := server.GetStoragePoolVolumes(config.Pool)
	if err != nil {
		return cmdMigrateData{}, err
	}

	volumeNames := []string{}
	for _, v := range volumes {
		if v.Type != "custom" {
			continue
		}

		volumeNames = append(volumeNames, v.Name)
	}

	for {
		volumeName, err := c.global.asker.AskString("Name of the new custom volume: ", "", nil)
		if err != nil {
			return cmdMigrateData{}, err
		}

		if slices.Contains(volumeNames, volumeName) {
			fmt.Printf("Storage volume %q already exists\n", volumeName)
			continue
		}

		config.CustomVolumeArgs.Name = volumeName
		break
	}

	err = c.askSourcePath(&config, migrationType)
	if err != nil {
		return cmdMigrateData{}, err
	}

	fmt.Println("\nCustom volume to be created:")

	scanner := bufio.NewScanner(strings.NewReader(config.renderCustomVolume()))
	for scanner.Scan() {
		fmt.Printf("  %s\n", scanner.Text())
	}

	shouldMigrate, err := c.global.asker.AskBool("Do you want to continue? [default=yes]: ", "yes")
	if err != nil {
		return cmdMigrateData{}, err
	}

	if !shouldMigrate {
		return cmdMigrateData{}, nil
	}

	return config, nil
}

func (c *cmdMigrate) migrateInstance(ctx context.Context, server incus.InstanceServer, migrationType MigrationType) error {
	if migrationType != MigrationTypeVM && migrationType != MigrationTypeContainer {
		return fmt.Errorf("Wrong migration type for migrateInstance")
	}

	config, err := c.gatherInstanceInfo(server, migrationType)
	if err != nil {
		return err
	}

	return c.runMigration(ctx, server, &config, migrationType, func(ctx context.Context, server incus.InstanceServer, config *cmdMigrateData, path string, migrationType MigrationType) error {
		// System architecture
		architectureName, err := osarch.ArchitectureGetLocal()
		if err != nil {
			return err
		}

		config.InstanceArgs.Architecture = architectureName

		reverter := revert.New()
		defer reverter.Fail()

		// Create the instance
		op, err := server.CreateInstance(config.InstanceArgs)
		if err != nil {
			return err
		}

		reverter.Add(func() {
			_, _ = server.DeleteInstance(config.InstanceArgs.Name)
		})

		progress := cli.ProgressRenderer{Format: "Transferring instance: %s"}
		_, err = op.AddHandler(progress.UpdateOp)
		if err != nil {
			progress.Done("")
			return err
		}

		err = transferRootfs(ctx, op, path, c.flagRsyncArgs, migrationType)
		if err != nil {
			return err
		}

		progress.Done(fmt.Sprintf("Instance %s successfully created", config.InstanceArgs.Name))
		reverter.Success()

		return nil
	})
}

func (c *cmdMigrate) migrateCustomVolume(ctx context.Context, server incus.InstanceServer, migrationType MigrationType) error {
	if migrationType != MigrationTypeVolumeBlock && migrationType != MigrationTypeVolumeFilesystem {
		return fmt.Errorf("Wrong migration type for migrateCustomVolume")
	}

	config, err := c.gatherCustomVolumeInfo(server, migrationType)
	if err != nil {
		return err
	}

	// User decided not to migrate.
	if config.CustomVolumeArgs.Name == "" {
		return nil
	}

	return c.runMigration(ctx, server, &config, migrationType, func(ctx context.Context, server incus.InstanceServer, config *cmdMigrateData, path string, migrationType MigrationType) error {
		reverter := revert.New()
		defer reverter.Fail()

		// Create the custom volume
		op, err := server.CreateStoragePoolVolumeFromMigration(config.Pool, config.CustomVolumeArgs)
		if err != nil {
			return err
		}

		reverter.Add(func() {
			_ = server.DeleteStoragePoolVolume(config.Pool, "custom", config.CustomVolumeArgs.Name)
		})

		progress := cli.ProgressRenderer{Format: "Transferring custom volume: %s"}
		_, err = op.AddHandler(progress.UpdateOp)
		if err != nil {
			progress.Done("")
			return err
		}

		err = transferRootfs(ctx, op, path, c.flagRsyncArgs, migrationType)
		if err != nil {
			return err
		}

		progress.Done(fmt.Sprintf("Custom volume %s successfully created", config.CustomVolumeArgs.Name))
		reverter.Success()

		return nil
	})
}

func (c *cmdMigrate) runMigration(ctx context.Context, server incus.InstanceServer, config *cmdMigrateData, migrationType MigrationType, migrationHandler func(ctx context.Context, server incus.InstanceServer, config *cmdMigrateData, path string, migrationType MigrationType) error) error {
	if config.Project != "" {
		server = server.UseProject(config.Project)
	}

	config.Mounts = append(config.Mounts, config.SourcePath)

	// Get and sort the mounts
	sort.Strings(config.Mounts)

	// Create the mount namespace and ensure we're not moved around
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Unshare a new mntns so our mounts don't leak
	err := unix.Unshare(unix.CLONE_NEWNS)
	if err != nil {
		return fmt.Errorf("Failed to unshare mount namespace: %w", err)
	}

	// Prevent mount propagation back to initial namespace
	err = unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
	if err != nil {
		return fmt.Errorf("Failed to disable mount propagation: %w", err)
	}

	// Create the temporary directory to be used for the mounts
	path, err := os.MkdirTemp("", "incus-migrate_mount_")
	if err != nil {
		return err
	}

	// Automatically clean-up the temporary path on exit
	defer func(path string) {
		// Unmount the path if it's a mountpoint.
		_ = unix.Unmount(path, unix.MNT_DETACH)
		_ = unix.Unmount(filepath.Join(path, "root.img"), unix.MNT_DETACH)

		// Cleanup VM image files.
		_ = os.Remove(filepath.Join(path, "converted-raw-image.img"))
		_ = os.Remove(filepath.Join(path, "root.img"))

		// Remove the directory itself.
		_ = os.Remove(path)
	}(path)

	var fullPath string

	if migrationType == MigrationTypeContainer || migrationType == MigrationTypeVolumeFilesystem {
		// Create the rootfs directory
		fullPath = fmt.Sprintf("%s/rootfs", path)

		err = os.Mkdir(fullPath, 0o755)
		if err != nil {
			return err
		}

		// Setup the source (mounts)
		err = setupSource(fullPath, config.Mounts)
		if err != nil {
			return fmt.Errorf("Failed to setup the source: %w", err)
		}
	} else {
		_, ext, convCmd, _ := archive.DetectCompression(config.SourcePath)
		if ext == ".qcow2" || ext == ".vmdk" {
			// COnfirm the command is available.
			_, err := exec.LookPath(convCmd[0])
			if err != nil {
				return fmt.Errorf("Unable to find required command %q", convCmd[0])
			}

			destImg := filepath.Join(path, "converted-raw-image.img")

			cmd := []string{
				"nice", "-n19", // Run with low priority to reduce CPU impact on other processes.
			}

			cmd = append(cmd, convCmd...)
			cmd = append(cmd, "-p", "-t", "writeback")

			// Check for Direct I/O support.
			from, err := os.OpenFile(config.SourcePath, unix.O_DIRECT|unix.O_RDONLY, 0)
			if err == nil {
				cmd = append(cmd, "-T", "none")
				_ = from.Close()
			}

			to, err := os.OpenFile(destImg, unix.O_DIRECT|unix.O_RDONLY, 0)
			if err == nil {
				cmd = append(cmd, "-t", "none")
				_ = to.Close()
			}

			cmd = append(cmd, config.SourcePath, destImg)

			fmt.Printf("Converting image %q to raw format before importing\n", config.SourcePath)

			c := exec.Command(cmd[0], cmd[1:]...)
			err = c.Run()
			if err != nil {
				return fmt.Errorf("Failed to convert image %q for importing: %w", config.SourcePath, err)
			}

			config.SourcePath = destImg
		}

		fullPath = path
		target := filepath.Join(path, "root.img")

		err = os.WriteFile(target, nil, 0o644)
		if err != nil {
			return fmt.Errorf("Failed to create %q: %w", target, err)
		}

		// Mount the path
		err = unix.Mount(config.SourcePath, target, "none", unix.MS_BIND, "")
		if err != nil {
			return fmt.Errorf("Failed to mount %s: %w", config.SourcePath, err)
		}

		// Make it read-only
		err = unix.Mount("", target, "none", unix.MS_BIND|unix.MS_RDONLY|unix.MS_REMOUNT, "")
		if err != nil {
			return fmt.Errorf("Failed to make %s read-only: %w", config.SourcePath, err)
		}
	}

	return migrationHandler(ctx, server, config, fullPath, migrationType)
}

func (c *cmdMigrate) run(_ *cobra.Command, _ []string) error {
	// Quick checks.
	if os.Geteuid() != 0 {
		return errors.New("This tool must be run as root")
	}

	_, err := exec.LookPath("rsync")
	if err != nil {
		return errors.New("Unable to find required command \"rsync\"")
	}

	// Server
	server, clientFingerprint, err := c.askServer()
	if err != nil {
		return err
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-sigChan

		if clientFingerprint != "" {
			_ = server.DeleteCertificate(clientFingerprint)
		}

		cancel()

		// The following nolint directive ignores the "deep-exit" rule of the revive linter.
		// We should be exiting cleanly by passing the above context into each invoked method and checking for
		// cancellation. Unfortunately our client methods do not accept a context argument.
		os.Exit(1) //nolint:revive
	}()

	if clientFingerprint != "" {
		defer func() { _ = server.DeleteCertificate(clientFingerprint) }()
	}

	// Provide migration type
	creationType, err := c.global.asker.AskInt(`
What would you like to create?
1) Container
2) Virtual Machine
3) Custom Volume (from filesystem)
4) Custom Volume (from disk)

Please enter the number of your choice: `, 1, 4, "", nil)
	if err != nil {
		return err
	}

	switch creationType {
	case 1:
		return c.migrateInstance(ctx, server, MigrationTypeContainer)
	case 2:
		return c.migrateInstance(ctx, server, MigrationTypeVM)
	case 3:
		return c.migrateCustomVolume(ctx, server, MigrationTypeVolumeFilesystem)
	case 4:
		return c.migrateCustomVolume(ctx, server, MigrationTypeVolumeBlock)
	}

	return nil
}

func (c *cmdMigrate) askProfiles(server incus.InstanceServer, config *cmdMigrateData) error {
	profileNames, err := server.GetProfileNames()
	if err != nil {
		return err
	}

	profiles, err := c.global.asker.AskString("Which profiles do you want to apply to the instance? (space separated) [default=default, \"-\" for none]: ", "default", func(s string) error {
		// This indicates that no profiles should be applied.
		if s == "-" {
			return nil
		}

		profiles := strings.Split(s, " ")

		for _, profile := range profiles {
			if !slices.Contains(profileNames, profile) {
				return fmt.Errorf("Unknown profile %q", profile)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	if profiles != "-" {
		config.InstanceArgs.Profiles = strings.Split(profiles, " ")
	}

	return nil
}

func (c *cmdMigrate) askConfig(config *cmdMigrateData) error {
	configs, err := c.global.asker.AskString("Please specify config keys and values (key=value ...): ", "", func(s string) error {
		if s == "" {
			return nil
		}

		for _, entry := range strings.Split(s, " ") {
			if !strings.Contains(entry, "=") {
				return fmt.Errorf("Bad key=value configuration: %v", entry)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, entry := range strings.Split(configs, " ") {
		key, value, _ := strings.Cut(entry, "=")
		config.InstanceArgs.Config[key] = value
	}

	return nil
}

func (c *cmdMigrate) askStorage(server incus.InstanceServer, config *cmdMigrateData) error {
	storagePools, err := server.GetStoragePoolNames()
	if err != nil {
		return err
	}

	if len(storagePools) == 0 {
		return fmt.Errorf("No storage pools available")
	}

	storagePool, err := c.global.asker.AskChoice("Please provide the storage pool to use: ", storagePools, "")
	if err != nil {
		return err
	}

	config.InstanceArgs.Devices["root"] = map[string]string{
		"type": "disk",
		"pool": storagePool,
		"path": "/",
	}

	changeStorageSize, err := c.global.asker.AskBool("Do you want to change the storage size? [default=no]: ", "no")
	if err != nil {
		return err
	}

	if changeStorageSize {
		size, err := c.global.asker.AskString("Please specify the storage size: ", "", func(s string) error {
			_, err := units.ParseByteSizeString(s)
			return err
		})
		if err != nil {
			return err
		}

		config.InstanceArgs.Devices["root"]["size"] = size
	}

	return nil
}

func (c *cmdMigrate) askNetwork(server incus.InstanceServer, config *cmdMigrateData) error {
	networks, err := server.GetNetworkNames()
	if err != nil {
		return err
	}

	network, err := c.global.asker.AskChoice("Please specify the network to use for the instance: ", networks, "")
	if err != nil {
		return err
	}

	config.InstanceArgs.Devices["eth0"] = map[string]string{
		"type":    "nic",
		"nictype": "bridged",
		"parent":  network,
		"name":    "eth0",
	}

	return nil
}

func (c *cmdMigrate) askProject(server incus.InstanceServer, config *cmdMigrateData) error {
	projectNames, err := server.GetProjectNames()
	if err != nil {
		return err
	}

	if len(projectNames) > 1 {
		project, err := c.global.asker.AskChoice("Project to create the instance in [default=default]: ", projectNames, api.ProjectDefaultName)
		if err != nil {
			return err
		}

		config.Project = project
		return nil
	}

	config.Project = api.ProjectDefaultName
	return nil
}

func (c *cmdMigrate) askSourcePath(config *cmdMigrateData, migrationType MigrationType) error {
	var question string
	var err error

	// Provide source path
	if migrationType == MigrationTypeVM || migrationType == MigrationTypeVolumeBlock {
		question = "Please provide the path to a disk, partition, or qcow2/raw/vmdk image file: "
	} else {
		question = "Please provide the path to a root filesystem: "
	}

	config.SourcePath, err = c.global.asker.AskString(question, "", func(s string) error {
		if !util.PathExists(s) {
			return errors.New("Path does not exist")
		}

		_, err := os.Stat(s)
		if err != nil {
			return err
		}

		// When migrating a disk, report the detected source format
		if migrationType == MigrationTypeVM || migrationType == MigrationTypeVolumeBlock {
			if linux.IsBlockdevPath(s) {
				config.SourceFormat = "Block device"
			} else if _, ext, _, _ := archive.DetectCompression(s); ext == ".qcow2" {
				config.SourceFormat = "qcow2"
			} else if _, ext, _, _ := archive.DetectCompression(s); ext == ".vmdk" {
				config.SourceFormat = "vmdk"
			} else {
				// If the input isn't a block device or qcow2/vmdk image, assume it's raw.
				// Positively identifying a raw image depends on parsing MBR/GPT partition tables.
				config.SourceFormat = "raw"
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}
