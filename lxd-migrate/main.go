package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxc/lxd/shared"
)

var argYes = flag.Bool("yes", false, "Answer yes to all questions")

func main() {
	flag.Parse()

	err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

func run() error {
	// Only run as root
	if os.Geteuid() != 0 {
		return fmt.Errorf("This tool must be run as root.")
	}

	// Confirm we're on Ubuntu
	if osID() != "ubuntu" {
		return fmt.Errorf("Data migration is only supported on Ubuntu at this time.")
	}

	// Validate that nothing depends on the current LXD
	err := packageRemovable("lxd")
	if err != nil {
		return err
	}

	err = packageRemovable("lxd-client")
	if err != nil {
		return err
	}

	// Connect to the source LXD
	fmt.Printf("=> Connecting to source server\n")
	src, err := lxdConnect("/var/lib/lxd")
	if err != nil {
		return fmt.Errorf("Unable to connect to the source LXD: %v", err)
	}

	// Connect to the destination LXD
	fmt.Printf("=> Connecting to destination server\n")
	dst, err := lxdConnect("/var/snap/lxd/common/lxd")
	if err != nil {
		return fmt.Errorf("Unable to connect to the destination LXD: %v", err)
	}

	// Sanity checks
	fmt.Printf("=> Running sanity checks\n")
	if compareVersions(src.info.Environment.ServerVersion, dst.info.Environment.ServerVersion) > 0 {
		return fmt.Errorf("The source server is running a more recent version than the destination.")
	}

	err = src.checkEmpty()
	if err == nil {
		fmt.Printf("The source server is empty, no migration needed.\n")
		return removePackages(src, dst)
	}

	err = dst.checkEmpty()
	if err != nil {
		return err
	}

	// Show migration report
	fmt.Printf("\n=== Source server\n")
	err = src.showReport()
	if err != nil {
		return err
	}

	fmt.Printf("\n=== Destination server\n")
	err = dst.showReport()
	if err != nil {
		return err
	}

	// Confirm that the user wants to go ahead
	fmt.Printf("\nThe migration process will shut down all your containers then move your data to the destination LXD.\n")
	fmt.Printf("Once the data is moved, the destination LXD will start and apply any needed updates.\n")
	fmt.Printf("And finally your containers will be brought back to their previous state, completing the migration.\n")

	isMnt := shared.IsMountPoint(src.path)
	if isMnt {
		fmt.Printf("\nWARNING: /var/lib/lxd is a mountpoint. You will need to update that mount location after the migration.\n")
	}

	fmt.Printf("\n")
	if !*argYes && !askBool("Are you ready to proceed (yes/no) [default=no]? ", "no") {
		return fmt.Errorf("Aborted by the user")
	}

	// Shutting down the daemons
	fmt.Printf("=> Shutting down the source LXD\n")
	err = src.shutdown()
	if err != nil {
		return fmt.Errorf("Failed to shutdown the source LXD: %v", err)
	}

	fmt.Printf("=> Stopping the source LXD units\n")
	err = src.stop()
	if err != nil {
		return fmt.Errorf("Failed to stop the source LXD units: %v", err)
	}

	fmt.Printf("=> Stopping the destination LXD unit\n")
	err = dst.stop()
	if err != nil {
		return fmt.Errorf("Failed to stop the destination LXD units: %v", err)
	}

	// Unmount any leftover mounts
	fmt.Printf("=> Unmounting source LXD paths\n")
	err = src.cleanMounts()
	if err != nil {
		return fmt.Errorf("Failed to unmount source LXD: %v", err)
	}

	fmt.Printf("=> Unmounting destination LXD paths\n")
	err = dst.cleanMounts()
	if err != nil {
		return fmt.Errorf("Failed to unmount destination LXD: %v", err)
	}

	// Wipe the destination LXD
	fmt.Printf("=> Wiping destination LXD clean\n")
	err = dst.wipe()
	if err != nil {
		return fmt.Errorf("Failed to wipe the destination LXD: %v", err)
	}

	// Move the data across
	if !isMnt {
		fmt.Printf("=> Moving the data\n")
		err = src.moveFiles(dst.path)
		if err != nil {
			return fmt.Errorf("Failed to move the data: %v", err)
		}
	} else {
		fmt.Printf("=> Moving the /var/lib/lxd mountpoint\n")
		err = src.remount(dst.path)
		if err != nil {
			return fmt.Errorf("Failed to move the mountpoint: %v", err)
		}
	}

	// Move the database into place
	fmt.Printf("=> Moving the database\n")
	os.Remove("/var/snap/lxd/current/lxd/lxd.db")
	os.MkdirAll("/var/snap/lxd/current/lxd/", 0755)

	_, err = shared.RunCommand("mv", filepath.Join(dst.path, "lxd.db"), "/var/snap/lxd/current/lxd/lxd.db")
	if err != nil {
		return fmt.Errorf("Failed to move the database into place: %v", err)
	}

	err = os.Symlink("../../current/lxd/lxd.db", filepath.Join(dst.path, "lxd.db"))
	if err != nil {
		return fmt.Errorf("Failed to create database symlink: %v", err)
	}

	// Access the database
	fmt.Printf("=> Backing up the database\n")
	err = shared.FileCopy("/var/snap/lxd/current/lxd/lxd.db", "/var/snap/lxd/current/lxd/lxd.db.pre-migration")
	if err != nil {
		return fmt.Errorf("Failed to backup the database: %v", err)
	}

	fmt.Printf("=> Opening the database\n")
	db, err := dbOpen(filepath.Join(dst.path, "lxd.db"))
	if err != nil {
		return fmt.Errorf("Failed to open the database: %v", err)
	}

	// Deal with the storage backends
	fmt.Printf("=> Updating the storage backends\n")
	err = src.rewriteStorage(db, dst.path)
	if err != nil {
		return fmt.Errorf("Failed to update the storage pools: %v", err)
	}

	// Copy the network config
	if src.networks == nil && dst.networks == nil {
		fmt.Printf("=> Moving bridge configuration\n")

		// Atempt to stop lxd-bridge
		systemdCtl("stop", "lxd-bridge")

		if shared.PathExists("/etc/default/lxd-bridge") {
			_, err = shared.RunCommand("mv", "/etc/default/lxd-bridge", "/var/snap/lxd/common/lxd-bridge/config")
			if err != nil {
				return fmt.Errorf("Failed to move the bridge configuration: %v", err)
			}
		}
	}

	// Start the destination LXD
	fmt.Printf("=> Starting the destination LXD\n")
	err = dst.start()
	if err != nil {
		return fmt.Errorf("Failed to start the destination LXD: %v", err)
	}

	// Wait for LXD to be online
	fmt.Printf("=> Waiting for LXD to come online\n")
	err = dst.wait()
	if err != nil {
		return err
	}

	if src.networks == nil && dst.networks != nil {
		// Update the network configuration
		fmt.Printf("=> Converting the network configuration\n")
		_, err = shared.RunCommand("upgrade-bridge")
		if err != nil {
			return fmt.Errorf("Failed to convert the network configuration: %v", err)
		}

		// Reload LXD post-update (to re-create the bridge if needed)
		fmt.Printf("=> Reloading LXD after network update")
		err = dst.reload()
		if err != nil {
			return err
		}

		// Wait for LXD to be online
		fmt.Printf("=> Waiting for LXD to come online\n")
		err = dst.wait()
		if err != nil {
			return err
		}
	}

	// Show the updated destination server
	fmt.Printf("\n=== Destination server\n")
	err = dst.update()
	if err != nil {
		return fmt.Errorf("Failed to update status of the destination LXD: %v", err)
	}

	err = dst.showReport()
	if err != nil {
		return err
	}

	// Mount warning
	if isMnt {
		fmt.Printf("\nWARNING: Make sure to update your system to mount your LXD directory at /var/snap/lxd/common/lxd\n")
	}

	return removePackages(src, dst)
}

func removePackages(src *lxdDaemon, dst *lxdDaemon) error {
	// Offer to remove LXD on the source
	fmt.Printf("\nThe migration is now complete and your containers should be back online.\n")
	if *argYes || askBool("Do you want to uninstall the old LXD (yes/no) [default=no]? ", "no") {
		err := src.uninstall()
		if err != nil {
			return err
		}
	}

	// Final message
	fmt.Printf("\nAll done. You may need to close your current shell and open a new one to have the \"lxc\" command work.\n")
	fmt.Printf("To migrate your existing client configuration, move ~/.config/lxc to ~/snap/lxd/current/.config/lxc\n")

	return nil
}

func askBool(question string, default_ string) bool {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Printf(question)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSuffix(input, "\n")
		if input == "" {
			input = default_
		}
		if shared.StringInSlice(strings.ToLower(input), []string{"yes", "y"}) {
			return true
		} else if shared.StringInSlice(strings.ToLower(input), []string{"no", "n"}) {
			return false
		}

		fmt.Printf("Invalid input, try again.\n\n")
	}
}
