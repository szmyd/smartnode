package node

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/urfave/cli"

	"github.com/rocket-pool/rocketpool-go/rocketpool"
	"github.com/rocket-pool/smartnode/shared/services"
	"github.com/rocket-pool/smartnode/shared/services/config"
	"github.com/rocket-pool/smartnode/shared/services/state"
	"github.com/rocket-pool/smartnode/shared/services/wallet/keystore/lighthouse"
	"github.com/rocket-pool/smartnode/shared/services/wallet/keystore/nimbus"
	"github.com/rocket-pool/smartnode/shared/services/wallet/keystore/prysm"
	"github.com/rocket-pool/smartnode/shared/services/wallet/keystore/teku"
	"github.com/rocket-pool/smartnode/shared/utils/log"
	"github.com/rocket-pool/smartnode/shared/utils/rp"
)

// Config
var tasksInterval, _ = time.ParseDuration("5m")
var taskCooldown, _ = time.ParseDuration("10s")

const (
	MaxConcurrentEth1Requests = 200

	ClaimRplRewardsColor         = color.FgGreen
	StakePrelaunchMinipoolsColor = color.FgBlue
	DownloadRewardsTreesColor    = color.FgGreen
	MetricsColor                 = color.FgHiYellow
	ManageFeeRecipientColor      = color.FgHiCyan
	PromoteMinipoolsColor        = color.FgMagenta
	ReduceBondAmountColor        = color.FgHiBlue
	ErrorColor                   = color.FgRed
	WarningColor                 = color.FgYellow
	UpdateColor                  = color.FgHiWhite
)

// Register node command
func RegisterCommands(app *cli.App, name string, aliases []string) {
	app.Commands = append(app.Commands, cli.Command{
		Name:    name,
		Aliases: aliases,
		Usage:   "Run Rocket Pool node activity daemon",
		Action: func(c *cli.Context) error {
			return run(c)
		},
	})
}

// Run daemon
func run(c *cli.Context) error {

	// Handle the initial fee recipient file deployment
	err := deployDefaultFeeRecipientFile(c)
	if err != nil {
		return err
	}

	// Clean up old fee recipient files
	err = removeLegacyFeeRecipientFiles(c)
	if err != nil {
		return err
	}

	// Configure
	configureHTTP()

	// Wait until node is registered
	if err := services.WaitNodeRegistered(c, true); err != nil {
		return err
	}

	// Get services
	cfg, err := services.GetConfig(c)
	if err != nil {
		return err
	}
	rp, err := services.GetRocketPool(c)
	if err != nil {
		return err
	}
	bc, err := services.GetBeaconClient(c)
	if err != nil {
		return err
	}

	// Initialize loggers
	errorLog := log.NewColorLogger(ErrorColor)
	updateLog := log.NewColorLogger(UpdateColor)

	// Create the state manager
	m, err := state.NewNetworkStateManager(rp, cfg, rp.Client, bc, &updateLog)
	if err != nil {
		return err
	}

	// Initialize tasks
	manageFeeRecipient, err := newManageFeeRecipient(c, log.NewColorLogger(ManageFeeRecipientColor), m)
	if err != nil {
		return err
	}
	stakePrelaunchMinipools, err := newStakePrelaunchMinipools(c, log.NewColorLogger(StakePrelaunchMinipoolsColor), m)
	if err != nil {
		return err
	}
	promoteMinipools, err := newPromoteMinipools(c, log.NewColorLogger(PromoteMinipoolsColor), m)
	if err != nil {
		return err
	}
	downloadRewardsTrees, err := newDownloadRewardsTrees(c, log.NewColorLogger(DownloadRewardsTreesColor), m)
	if err != nil {
		return err
	}
	reduceBonds, err := newReduceBonds(c, log.NewColorLogger(ReduceBondAmountColor), m)
	if err != nil {
		return err
	}

	// Wait group to handle the various threads
	wg := new(sync.WaitGroup)
	wg.Add(2)

	// Run task loop
	isAtlasDeployedMasterFlag := false
	go func() {
		for {
			// Check the EC status
			err := services.WaitEthClientSynced(c, false) // Force refresh the primary / fallback EC status
			if err != nil {
				errorLog.Println(err)
				time.Sleep(taskCooldown)
				continue
			}

			// Check the BC status
			err = services.WaitBeaconClientSynced(c, false) // Force refresh the primary / fallback BC status
			if err != nil {
				errorLog.Println(err)
				time.Sleep(taskCooldown)
				continue
			}

			// Check for Atlas
			if !isAtlasDeployedMasterFlag {
				isAtlasDeployed, err := checkIfAtlasIsDeployed(rp)
				if err != nil {
					errorLog.Println(err)
					time.Sleep(taskCooldown)
					continue
				}
				isAtlasDeployedMasterFlag = isAtlasDeployed
			}

			// Update the network state
			if err := updateNetworkState(m, updateLog, isAtlasDeployedMasterFlag); err != nil {
				errorLog.Println(err)
				time.Sleep(taskCooldown)
				continue
			}

			// Manage the fee recipient for the node
			if err := manageFeeRecipient.run(isAtlasDeployedMasterFlag); err != nil {
				errorLog.Println(err)
			}
			time.Sleep(taskCooldown)

			// Run the rewards download check
			if err := downloadRewardsTrees.run(isAtlasDeployedMasterFlag); err != nil {
				errorLog.Println(err)
			}
			time.Sleep(taskCooldown)

			// Run the minipool stake check
			if err := stakePrelaunchMinipools.run(isAtlasDeployedMasterFlag); err != nil {
				errorLog.Println(err)
			}
			time.Sleep(taskCooldown)

			// Run the reduce bond check
			if err := reduceBonds.run(isAtlasDeployedMasterFlag); err != nil {
				errorLog.Println(err)
			}
			time.Sleep(taskCooldown)

			// Run the minipool promotion check
			if err := promoteMinipools.run(isAtlasDeployedMasterFlag); err != nil {
				errorLog.Println(err)
			}

			time.Sleep(tasksInterval)
		}
		wg.Done()
	}()

	// Run metrics loop
	go func() {
		err := runMetricsServer(c, log.NewColorLogger(MetricsColor), m)
		if err != nil {
			errorLog.Println(err)
		}
		wg.Done()
	}()

	// Wait for both threads to stop
	wg.Wait()
	return nil

}

// Configure HTTP transport settings
func configureHTTP() {

	// The watchtower daemon makes a large number of concurrent RPC requests to the Eth1 client
	// The HTTP transport is set to cache connections for future re-use equal to the maximum expected number of concurrent requests
	// This prevents issues related to memory consumption and address allowance from repeatedly opening and closing connections
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = MaxConcurrentEth1Requests

}

// Copy the default fee recipient file into the proper location
func deployDefaultFeeRecipientFile(c *cli.Context) error {

	cfg, err := services.GetConfig(c)
	if err != nil {
		return err
	}

	feeRecipientPath := cfg.Smartnode.GetFeeRecipientFilePath()
	_, err = os.Stat(feeRecipientPath)
	if os.IsNotExist(err) {
		// Make sure the validators dir is created
		validatorsFolder := filepath.Dir(feeRecipientPath)
		err = os.MkdirAll(validatorsFolder, 0755)
		if err != nil {
			return fmt.Errorf("could not create validators directory: %w", err)
		}

		// Create the file
		var defaultFeeRecipientFileContents string
		if cfg.IsNativeMode {
			// Native mode needs an environment variable definition
			defaultFeeRecipientFileContents = fmt.Sprintf("%s=%s", config.FeeRecipientEnvVar, cfg.Smartnode.GetRethAddress().Hex())
		} else {
			// Docker and Hybrid just need the address itself
			defaultFeeRecipientFileContents = cfg.Smartnode.GetRethAddress().Hex()
		}
		err := ioutil.WriteFile(feeRecipientPath, []byte(defaultFeeRecipientFileContents), 0664)
		if err != nil {
			return fmt.Errorf("could not write default fee recipient file to %s: %w", feeRecipientPath, err)
		}
	} else if err != nil {
		return fmt.Errorf("Error checking fee recipient file status: %w", err)
	}

	return nil

}

// Remove the old fee recipient files that were created in v1.5.0
func removeLegacyFeeRecipientFiles(c *cli.Context) error {

	legacyFeeRecipientFile := "rp-fee-recipient.txt"

	cfg, err := services.GetConfig(c)
	if err != nil {
		return err
	}

	validatorsFolder := cfg.Smartnode.GetValidatorKeychainPath()

	// Remove the legacy files
	keystoreDirs := []string{lighthouse.KeystoreDir, nimbus.KeystoreDir, prysm.KeystoreDir, teku.KeystoreDir}
	for _, keystoreDir := range keystoreDirs {
		oldFile := filepath.Join(validatorsFolder, keystoreDir, legacyFeeRecipientFile)
		_, err = os.Stat(oldFile)
		if !os.IsNotExist(err) {
			err = os.Remove(oldFile)
			if err != nil {
				fmt.Printf("NOTE: Couldn't remove old fee recipient file (%s): %s\nThis file is no longer used, you may remove it manually if you wish.\n", oldFile, err.Error())
			}
		}
	}

	return nil

}

// Check if Atlas has been deployed yet
func checkIfAtlasIsDeployed(rpbinding *rocketpool.RocketPool) (bool, error) {
	isAtlasDeployed, err := rp.IsAtlasDeployed(rpbinding)
	if err != nil {
		return false, fmt.Errorf("error checking if Atlas is deployed: %w", err)
	}

	if isAtlasDeployed {
		fmt.Println(`
*       .
*      / \
*     |.'.|
*     |'.'|
*   ,'|   |'.
*  |,-'-|-'-.|
*   __|_| |         _        _      _____           _
*  | ___ \|        | |      | |    | ___ \         | |
*  | |_/ /|__   ___| | _____| |_   | |_/ /__   ___ | |
*  |    // _ \ / __| |/ / _ \ __|  |  __/ _ \ / _ \| |
*  | |\ \ (_) | (__|   <  __/ |_   | | | (_) | (_) | |
*  \_| \_\___/ \___|_|\_\___|\__|  \_|  \___/ \___/|_|
* +---------------------------------------------------+
* |    DECENTRALISED STAKING PROTOCOL FOR ETHEREUM    |
* +---------------------------------------------------+
*
* ================ Atlas has launched! ================
`)
	}
	return isAtlasDeployed, nil
}

// Update the latest network state at each cycle
func updateNetworkState(m *state.NetworkStateManager, log log.ColorLogger, isAtlasDeployed bool) error {
	log.Print("Getting latest network state... ")
	start := time.Now()

	// Get the state of the network
	_, err := m.UpdateState(nil, isAtlasDeployed)
	if err != nil {
		return fmt.Errorf("error updating network state: %w", err)
	}

	log.Printlnf("done in %s", time.Since(start))
	return nil
}
