package thorchain_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cosmossdk.io/math"
	ethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/strangelove-ventures/interchaintest/v8"
	"github.com/strangelove-ventures/interchaintest/v8/chain/cosmos"
	"github.com/strangelove-ventures/interchaintest/v8/chain/ethereum"
	tc "github.com/strangelove-ventures/interchaintest/v8/chain/thorchain"
	"github.com/strangelove-ventures/interchaintest/v8/chain/utxo"
	"github.com/strangelove-ventures/interchaintest/v8/chain/thorchain/common"
	"github.com/strangelove-ventures/interchaintest/v8/examples/thorchain/features"
	"github.com/strangelove-ventures/interchaintest/v8/ibc"

	//"github.com/strangelove-ventures/interchaintest/v8/ibc"
	"github.com/strangelove-ventures/interchaintest/v8/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestThorchain(t *testing.T) {
	numThorchainValidators := 1
	numThorchainFullNodes  := 0

	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	t.Parallel()

	// ------------------------
	// Setup EVM chains first
	// ------------------------
	ethChainName := common.ETHChain.String() // must use this name for test
	btcChainName := common.BTCChain.String() // must use this name for test 
	bchChainName := common.BCHChain.String() // must use this name for test 
	liteChainName := common.LTCChain.String() // must use this name for test 
	dogeChainName := common.DOGEChain.String() // must use this name for test 
	
	// TODO: return default chainspec instead of chain config
	cf0 := interchaintest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*interchaintest.ChainSpec{
		{
			ChainName:   ethChainName,
			Name:        ethChainName,
			Version:     "latest",
			ChainConfig: ethereum.DefaultEthereumAnvilChainConfig(ethChainName),
		},
		{
			ChainName:   btcChainName,
			Name:        btcChainName,
			Version:     "26.2",
			ChainConfig: utxo.DefaultBitcoinChainConfig(btcChainName, "thorchain", "password"),
		},
		{
			ChainName:   bchChainName,
			Name:        bchChainName,
			Version:     "27.1.0",
			ChainConfig: utxo.DefaultBitcoinCashChainConfig(bchChainName, "thorchain", "password"),
		},
		{
			ChainName: liteChainName,
			Name:      liteChainName,
			Version:   "0.21",
			ChainConfig: utxo.DefaultLitecoinChainConfig(liteChainName, "thorchain", "password"),
		},
		{
			ChainName: dogeChainName,
			Name:      dogeChainName,
			Version:   "dogecoin-daemon-1.14.7",
			ChainConfig: utxo.DefaultDogecoinChainConfig(dogeChainName, "thorchain", "password"),
		},
	})

	chains, err := cf0.Chains(t.Name())
	require.NoError(t, err)
	ethChain := chains[0].(*ethereum.EthereumChain)
	btcChain := chains[1].(*utxo.UtxoChain)
	bchChain := chains[2].(*utxo.UtxoChain)
	liteChain := chains[3].(*utxo.UtxoChain)
	dogeChain := chains[4].(*utxo.UtxoChain)

	btcChain.UnloadWalletAfterUse(true)
	bchChain.UnloadWalletAfterUse(true)
	liteChain.UnloadWalletAfterUse(true)

	ic0 := interchaintest.NewInterchain().
		AddChain(ethChain).
		AddChain(btcChain).
		AddChain(bchChain).
		AddChain(liteChain).
		AddChain(dogeChain)
	
	ctx := context.Background()
	client, network := interchaintest.DockerSetup(t)

	require.NoError(t, ic0.Build(ctx, nil, interchaintest.InterchainBuildOptions{
		TestName:         t.Name(),
		Client:           client,
		NetworkID:        network,
		SkipPathCreation: true,
	}))
	t.Cleanup(func() {
		_ = ic0.Close()
	})

	ethUserInitialAmount := ethereum.ETHER.MulRaw(2)

	ethUser, err := interchaintest.GetAndFundTestUserWithMnemonic(ctx, "user", strings.Repeat("dog ", 23) + "fossil", ethUserInitialAmount, ethChain)
	require.NoError(t, err)

	//ethChain.SendFunds(ctx, "faucet", ibc.WalletAmount{
	//	Address: "0x1804c8ab1f12e6bbf3894d4083f33e07309d1f38",
	//	Amount: math.NewInt(ethereum.ETHER),
	//})

	//os.Setenv("ETHFAUCET", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	stdout, _, err := ethChain.ForgeScript(ctx, ethUser.KeyName(), ethereum.ForgeScriptOpts{
		ContractRootDir: "contracts",
		SolidityContract: "script/Token.s.sol",
		RawOptions:       []string{"--sender", ethUser.FormattedAddress(), "--json"},
	})
	require.NoError(t, err)

	tokenContractAddress, err := GetEthAddressFromStdout(string(stdout))
	require.NoError(t, err)
	require.NotEmpty(t, tokenContractAddress)
	require.True(t, ethcommon.IsHexAddress(tokenContractAddress))

	fmt.Println("Token contract address:", tokenContractAddress)

	stdout, _, err = ethChain.ForgeScript(ctx, ethUser.KeyName(), ethereum.ForgeScriptOpts{
		ContractRootDir: "contracts",
		SolidityContract: "script/Router.s.sol",
		RawOptions:       []string{"--sender", ethUser.FormattedAddress(), "--json"},
	})
	require.NoError(t, err)

	ethRouterContractAddress, err := GetEthAddressFromStdout(string(stdout))
	require.NoError(t, err)
	require.NotEmpty(t, ethRouterContractAddress)
	require.True(t, ethcommon.IsHexAddress(ethRouterContractAddress))

	fmt.Println("Router contract address:", ethRouterContractAddress)


	// ----------------------------
	// Set up thorchain and others
	// ----------------------------
	thorchainChainSpec := ThorchainDefaultChainSpec(t.Name(), numThorchainValidators, numThorchainFullNodes, ethRouterContractAddress)
	// TODO: add router contracts to thorchain
	// Set ethereum RPC
	// Move other chains to above for setup too?
	//thorchainChainSpec.
	chainSpecs := []*interchaintest.ChainSpec{
		thorchainChainSpec,
		GaiaChainSpec(),
	}

	cf := interchaintest.NewBuiltinChainFactory(zaptest.NewLogger(t), chainSpecs)

	chains, err = cf.Chains(t.Name())
	require.NoError(t, err)

	thorchain := chains[0].(*tc.Thorchain)
	gaia := chains[1].(*cosmos.CosmosChain)

	ic := interchaintest.NewInterchain().
		AddChain(thorchain).
		AddChain(gaia)
	
	require.NoError(t, ic.Build(ctx, nil, interchaintest.InterchainBuildOptions{
		TestName:         t.Name(),
		Client:           client,
		NetworkID:        network,
		SkipPathCreation: true,
	}))
	t.Cleanup(func() {
		_ = ic.Close()
	})

	err = gaia.SendFunds(ctx, "faucet", ibc.WalletAmount{
		Address: "cosmos1zf3gsk7edzwl9syyefvfhle37cjtql35427vcp",
		Denom: gaia.Config().Denom,
		Amount: math.NewInt(10000000),
	})

	err = thorchain.StartAllValSidecars(ctx)
	require.NoError(t, err, "failed starting validator sidecars")

	doTxs(t, ctx, gaia) // Do 100 transactions

	defaultFundAmount := math.NewInt(100_000_000)
	users := interchaintest.GetAndFundTestUsers(t, ctx, "default", defaultFundAmount, thorchain)
	thorchainUser := users[0]
	err = testutil.WaitForBlocks(ctx, 2, thorchain)
	require.NoError(t, err, "thorchain failed to make blocks")

	// --------------------------------------------------------
	// Check balances are correct
	// --------------------------------------------------------
	thorchainUserAmount, err := thorchain.GetBalance(ctx, thorchainUser.FormattedAddress(), thorchain.Config().Denom)
	require.NoError(t, err)
	require.True(t, thorchainUserAmount.Equal(defaultFundAmount), "Initial thorchain user amount not expected")
	
	val0 := thorchain.GetNode()
	faucetAddr, err := val0.AccountKeyBech32(ctx, "faucet")
	require.NoError(t, err)
	faucetAmount, err := thorchain.GetBalance(ctx, faucetAddr, thorchain.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, InitialFaucetAmount.Sub(defaultFundAmount).Sub(StaticGas), faucetAmount)

	// TODO: Run each stage in parallel

	// --------------------------------------------------------
	// Bootstrap pool
	// --------------------------------------------------------
	_, gaiaLper := features.DualLp(t, ctx, thorchain, gaia)
	_, ethLper := features.DualLp(t, ctx, thorchain, ethChain)
	_, btcLper := features.DualLp(t, ctx, thorchain, btcChain)
	_, bchLper := features.DualLp(t, ctx, thorchain, bchChain)
	_, liteLper := features.DualLp(t, ctx, thorchain, liteChain)
	_, dogeLper := features.DualLp(t, ctx, thorchain, dogeChain)

	// --------------------------------------------------------
	// Savers
	// --------------------------------------------------------
	gaiaSaver := features.Saver(t, ctx, thorchain, gaia)
	ethSaver := features.Saver(t, ctx, thorchain, ethChain)
	btcSaver := features.Saver(t, ctx, thorchain, btcChain)
	bchSaver := features.Saver(t, ctx, thorchain, bchChain)
	liteSaver := features.Saver(t, ctx, thorchain, liteChain)
	dogeSaver := features.Saver(t, ctx, thorchain, dogeChain)
	
	// --------------------------------------------------------
	// Arb
	// --------------------------------------------------------
	_, err = features.Arb(t, ctx, thorchain, 
		gaia, ethChain, btcChain, bchChain, liteChain, dogeChain) // Must add all active chains
	require.NoError(t, err)
	
	// --------------------------------------------------------
	// Swap
	// --------------------------------------------------------
	//err = features.SingleSwap(t, ctx, thorchain, gaia, thorchain)
	//require.NoError(t, err)
	
	err = features.SingleSwap(t, ctx, thorchain, ethChain, gaia)
	require.NoError(t, err)
	err = features.SingleSwap(t, ctx, thorchain, gaia, ethChain)
	require.NoError(t, err)
	
	err = features.SingleSwap(t, ctx, thorchain, btcChain, bchChain)
	require.NoError(t, err)
	err = features.SingleSwap(t, ctx, thorchain, bchChain, btcChain)
	require.NoError(t, err)

	err = features.SingleSwap(t, ctx, thorchain, dogeChain, liteChain)
	require.NoError(t, err)
	err = features.SingleSwap(t, ctx, thorchain, liteChain, dogeChain)
	require.NoError(t, err)
	
	// --------------------------------------------------------
	// Saver Eject
	// --------------------------------------------------------
	_ = features.SaverEject(t, ctx, thorchain, ethChain, ethSaver)
	_ = features.SaverEject(t, ctx, thorchain, gaia, gaiaSaver)
	_ = features.SaverEject(t, ctx, thorchain, btcChain, btcSaver)
	_ = features.SaverEject(t, ctx, thorchain, bchChain, bchSaver)
	_ = features.SaverEject(t, ctx, thorchain, liteChain, liteSaver)
	_ = features.SaverEject(t, ctx, thorchain, dogeChain, dogeSaver)
	
	// --------------------------------------------------------
	// Ragnarok
	// --------------------------------------------------------
	err = features.Ragnarok(t, ctx, thorchain, gaia, gaiaLper, gaiaSaver)
	require.NoError(t, err)
	err = features.Ragnarok(t, ctx, thorchain, ethChain, ethLper, ethSaver)
	require.NoError(t, err)
	err = features.Ragnarok(t, ctx, thorchain, btcChain, btcLper, btcSaver)
	require.NoError(t, err)
	err = features.Ragnarok(t, ctx, thorchain, bchChain, bchLper, bchSaver)
	require.NoError(t, err)
	err = features.Ragnarok(t, ctx, thorchain, liteChain, liteLper, liteSaver)
	require.NoError(t, err)
	err = features.Ragnarok(t, ctx, thorchain, dogeChain, dogeLper, dogeSaver)
	require.NoError(t, err)
	
	//err = gaia.StopAllNodes(ctx)
	//require.NoError(t, err)

	//state, err := gaia.ExportState(ctx, -1)
	//require.NoError(t, err)
	//fmt.Println("State: ", state)


	//err = testutil.WaitForBlocks(ctx, 300, thorchain)
	//require.NoError(t, err, "thorchain failed to make blocks")
}
