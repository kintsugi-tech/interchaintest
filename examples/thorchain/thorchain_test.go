package thorchain_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"cosmossdk.io/math"

	"github.com/strangelove-ventures/interchaintest/v8"
	"github.com/strangelove-ventures/interchaintest/v8/chain/cosmos"
	tc "github.com/strangelove-ventures/interchaintest/v8/chain/thorchain"
	"github.com/strangelove-ventures/interchaintest/v8/chain/thorchain/common"
	"github.com/strangelove-ventures/interchaintest/v8/ibc"
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

	chainSpecs := []*interchaintest.ChainSpec{
		ThorchainDefaultChainSpec(t.Name(), numThorchainValidators, numThorchainFullNodes),
		GaiaChainSpec(),
	}

	cf := interchaintest.NewBuiltinChainFactory(zaptest.NewLogger(t), chainSpecs)

	chains, err := cf.Chains(t.Name())
	require.NoError(t, err)

	thorchain := chains[0].(*tc.Thorchain)
	gaia := chains[1].(*cosmos.CosmosChain)

	ic := interchaintest.NewInterchain().
		AddChain(thorchain).
		AddChain(gaia)
	
	ctx := context.Background()
	client, network := interchaintest.DockerSetup(t)

	require.NoError(t, ic.Build(ctx, nil, interchaintest.InterchainBuildOptions{
		TestName:         t.Name(),
		Client:           client,
		NetworkID:        network,
		SkipPathCreation: true,
	}))
	t.Cleanup(func() {
		_ = ic.Close()
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

	// --------------------------------------------------------
	// Bootstrap pool
	// --------------------------------------------------------
	lperFundAmount := math.NewInt(100_000_000_000) // 100k (gaia), 1k (thorchain)
	users = interchaintest.GetAndFundTestUsers(t, ctx, "lpers", lperFundAmount, thorchain, gaia)
	thorchainLper := users[0]
	gaiaLper := users[1]

	pools, err := thorchain.ApiGetPools()
	require.NoError(t, err)
	require.Equal(t, 0 ,len(pools))

	// deposit atom
	memo := fmt.Sprintf("+:%s:%s", "GAIA.ATOM", thorchainLper.FormattedAddress())
	amount := math.NewInt(90_000_000_000)
	gaiaInboundAddr, _, err := thorchain.ApiGetInboundAddress("GAIA")
	require.NoError(t, err)
	_, err = gaia.GetNode().BankSendWithMemo(ctx, gaiaLper.KeyName(), ibc.WalletAmount{
		Address: gaiaInboundAddr,
		Denom: gaia.Config().Denom,
		Amount: amount,
	}, memo)
	require.NoError(t, err)

	// deposit rune
	memo = fmt.Sprintf("+:%s:%s", "GAIA.ATOM", gaiaLper.FormattedAddress())
	err = thorchain.Deposit(ctx, thorchainLper.KeyName(), amount, thorchain.Config().Denom, memo)
	require.NoError(t, err)

	pools, err = thorchain.ApiGetPools()
	require.NoError(t, err)
	count := 0
	for len(pools) < 1 {
		time.Sleep(time.Second)
		pools, err = thorchain.ApiGetPools()
		require.NoError(t, err)
		count++
		require.Less(t, count, 30, "atom pool didn't get set up in 30 seconds")
	}

	// --------------------------------------------------------
	// Savers
	// --------------------------------------------------------
	saversFundAmount := math.NewInt(100_000_000_000)
	users = interchaintest.GetAndFundTestUsers(t, ctx, "savers", saversFundAmount, thorchain, gaia)
	_, gaiaSaver := users[0], users[1]

	pool, err := thorchain.ApiGetPool(common.ATOMAsset)
	require.NoError(t, err)
	saveAmount := math.NewUintFromString(pool.BalanceAsset).
		MulUint64(500).QuoUint64(10_000)

	saverQuote, err := thorchain.ApiGetSaverDepositQuote(common.ATOMAsset, saveAmount)
	require.NoError(t, err)
	
	// store expected range to fail if received amount is outside 5% tolerance
	quoteOut := math.NewUintFromString(saverQuote.ExpectedAmountDeposit)
	tolerance := quoteOut.QuoUint64(20)
	if saverQuote.Fees.Outbound != nil {
		outboundFee := math.NewUintFromString(*saverQuote.Fees.Outbound)
		quoteOut = quoteOut.Add(outboundFee)
	}
	minExpectedSaver := quoteOut.Sub(tolerance)
	maxExpectedSaver := quoteOut.Add(tolerance)

	// Alternate between memo and memoless
	memo = fmt.Sprintf("+:%s", "GAIA/ATOM")
	//memo = ""
	gaiaInboundAddr, _, err = thorchain.ApiGetInboundAddress("GAIA")
	require.NoError(t, err)
	_, err = gaia.GetNode().BankSendWithMemo(ctx, gaiaSaver.KeyName(), ibc.WalletAmount{
		Address: gaiaInboundAddr,
		Denom: gaia.Config().Denom,
		Amount: math.Int(saveAmount).QuoRaw(100), // save amount is based on 8 dec
	}, memo)

	saverFound := false
	for count := 0; !saverFound; count++ {
		time.Sleep(time.Second)
		savers, err := thorchain.ApiGetSavers(common.ATOMAsset)
		require.NoError(t, err)
		for _, saver := range savers {
			if saver.AssetAddress != gaiaSaver.FormattedAddress() {
				continue
			}
			saverFound = true
			deposit := math.NewUintFromString(saver.AssetDepositValue)
			require.True(t, deposit.GTE(minExpectedSaver), fmt.Sprintf("Actual: %s, Min expected: %s", deposit, minExpectedSaver))
			require.True(t, deposit.LTE(maxExpectedSaver), fmt.Sprintf("Actual: %s, Max expected: %s", deposit, maxExpectedSaver))
		}
		require.Less(t, count, 30, "saver took longer than 30 sec to show")
	}

	// --------------------------------------------------------
	// Arb (implement after adding another chain)
	// --------------------------------------------------------
	err = thorchain.RecoverKey(ctx, "admin", strings.Repeat("master ", 23) + "notice")
	require.NoError(t, err)

	arbFundAmount := math.NewInt(1_000_000_000) // 1k (gaia), 10 (thorchain)
	users = interchaintest.GetAndFundTestUsers(t, ctx, "arb", arbFundAmount, thorchain, gaia)
	thorchainArber := users[0]
	gaiaArber := users[1]
	
	mimirs, err := thorchain.ApiGetMimirs()
	require.NoError(t, err)

	if mimir, ok := mimirs["TradeAccountsEnabled"]; (ok && mimir != int64(1) || !ok) {
		err := thorchain.SetMimir(ctx, "admin", "TradeAccountsEnabled", "1")
		require.NoError(t, err)
	}

	memo = fmt.Sprintf("trade+:%s", thorchainArber.FormattedAddress())
	gaiaInboundAddr, _, err = thorchain.ApiGetInboundAddress("GAIA")
	require.NoError(t, err)
	_, err = gaia.GetNode().BankSendWithMemo(ctx, gaiaArber.KeyName(), ibc.WalletAmount{
		Address: gaiaInboundAddr,
		Denom: gaia.Config().Denom,
		Amount: arbFundAmount.QuoRaw(10).MulRaw(9),
	}, memo)

	go func() {
		type Pool struct {
			BalanceRune math.Uint
			BalanceAsset math.Uint
		}
		originalPools := make(map[string]Pool)
		maxBasisPts := uint64(10_000)

		for {
			pools, err = thorchain.ApiGetPools()
			require.NoError(t, err)

			allPoolsSuspended := true
			arbPools := []tc.Pool{}
			for _, pool := range pools {
				if pool.Status != "Suspended" {
					allPoolsSuspended = false
				}

				// skip unavailable pools and those with no liquidity
				if pool.BalanceRune == "0" || pool.BalanceAsset == "0" || pool.Status != "Available" {
					continue
				}

				// if this is the first time we see the pool, store it to use as the target price
				if _, ok := originalPools[pool.Asset]; !ok {
					originalPools[pool.Asset] = Pool{
						BalanceRune:  math.NewUintFromString(pool.BalanceRune),
						BalanceAsset: math.NewUintFromString(pool.BalanceAsset),
					}
					continue
				}

				arbPools = append(arbPools, pool)
			}

			if allPoolsSuspended {
				return
			}

			if len(arbPools) < 2 {
				time.Sleep(time.Second * 5)
				continue
			}

			// sort pools by price change
			priceChangeBps := func(pool tc.Pool) int64 {
				originalPool := originalPools[pool.Asset]
				originalPrice := originalPool.BalanceRune.MulUint64(1e8).Quo(originalPool.BalanceAsset)
				currentPrice := math.NewUintFromString(pool.BalanceRune).MulUint64(1e8).Quo(math.NewUintFromString(pool.BalanceAsset))
				return int64(maxBasisPts) - int64(originalPrice.MulUint64(maxBasisPts).Quo(currentPrice).Uint64())
			}
			sort.Slice(arbPools, func(i, j int) bool {
				return priceChangeBps(arbPools[i]) > priceChangeBps(arbPools[j])
			})

			send := arbPools[0]
			receive := arbPools[len(arbPools)-1]

			// skip if none have diverged more than 10 basis points
			adjustmentBps := Min(Abs(priceChangeBps(send)), Abs(priceChangeBps(receive)))
			if adjustmentBps < 10 {
				// pools have not diverged enough
				time.Sleep(time.Second * 5)
				continue
			}

			// build the swap
			memo := fmt.Sprintf("=:%s", strings.Replace(receive.Asset, ".", "~", 1))
			asset, err := common.NewAsset(strings.Replace(send.Asset, ".", "~", 1))
			require.NoError(t, err)
			amount := math.NewUint(uint64(adjustmentBps / 2)).Mul(math.NewUintFromString(send.BalanceAsset)).QuoUint64(maxBasisPts)

			err = thorchain.Deposit(ctx, thorchainArber.KeyName(), math.Int(amount), asset.String(), memo)
			require.NoError(t, err)

			time.Sleep(time.Second * 5)
		}
	}()

	// --------------------------------------------------------
	// Swap
	// --------------------------------------------------------
	swapperFundAmount := math.NewInt(1_000_000_000) // 1k (gaia), 10 (thorchain)
	users = interchaintest.GetAndFundTestUsers(t, ctx, "swappers", swapperFundAmount, thorchain, gaia)
	thorchainSwapper := users[0]
	gaiaSwapper := users[1]
	
	// Get quote and calculate expected min/max output
	swapAmountAtomToRune := math.NewUint(500_000_000)
	swapQuote, err := thorchain.ApiGetSwapQuote(common.ATOMAsset, common.RuneNative, swapAmountAtomToRune.MulUint64(100)) // Thorchain has 8 dec for atom
	
	// store expected range to fail if received amount is outside 5% tolerance
	quoteOut = math.NewUintFromString(swapQuote.ExpectedAmountOut)
	tolerance = quoteOut.QuoUint64(20)
	if swapQuote.Fees.Outbound != nil {
		outboundFee := math.NewUintFromString(*swapQuote.Fees.Outbound)
		quoteOut = quoteOut.Add(outboundFee)

		// handle 2x gas rate fluctuation (add 1x outbound fee to tolerance)
		tolerance = tolerance.Add(outboundFee)
	}
	minExpectedRune := quoteOut.Sub(tolerance)
	maxExpectedRune := quoteOut.Add(tolerance)

	gaiaInboundAddr, _, err = thorchain.ApiGetInboundAddress("GAIA")
	require.NoError(t, err)
	memo = fmt.Sprintf("=:%s:%s", common.RuneNative.String(), thorchainSwapper.FormattedAddress())
	txHash, err := gaia.GetNode().BankSendWithMemo(ctx, gaiaSwapper.KeyName(), ibc.WalletAmount{
		Address: gaiaInboundAddr,
		Denom: gaia.Config().Denom,
		Amount: math.Int(swapAmountAtomToRune),
	}, memo)
	require.NoError(t, err)

	// ----- VerifyOutbound -----
	stages, err := thorchain.ApiGetTxStages(txHash)
	require.NoError(t, err)
	count = 0
	for stages.SwapFinalised == nil || !stages.SwapFinalised.Completed {
	//for stages.OutboundSigned == nil || !stages.OutboundSigned.Completed { // Only for non-rune swaps
		time.Sleep(time.Second)
		stages, err = thorchain.ApiGetTxStages(txHash)
		require.NoError(t, err)
		count++
		require.Less(t, count, 60, "swap didn't complete in 60 seconds")
	}

	details, err := thorchain.ApiGetTxDetails(txHash)
	require.NoError(t, err)
	require.Equal(t, 1, len(details.OutTxs))
	require.Equal(t, 1, len(details.Actions))

	// verify outbound amount + max gas within expected range
	action := details.Actions[0]
	out := details.OutTxs[0]
	outAmountPlusMaxGas := math.NewUintFromString(out.Coins[0].Amount)
	maxGas := action.MaxGas[0]
	if maxGas.Asset == common.RuneNative.String() {
		outAmountPlusMaxGas = outAmountPlusMaxGas.Add(math.NewUintFromString(maxGas.Amount))
	} else { // shouldn't enter here for atom -> rune
		var maxGasAssetValue math.Uint
		maxGasAssetValue, err = thorchain.ConvertAssetAmount(maxGas, common.RuneNative.String())
		require.NoError(t, err)
		outAmountPlusMaxGas = outAmountPlusMaxGas.Add(maxGasAssetValue)
	}

	thorchainSwapperBalance, err := thorchain.GetBalance(ctx, thorchainSwapper.FormattedAddress(), thorchain.Config().Denom)
	require.NoError(t, err)
	actualRune := thorchainSwapperBalance.Sub(swapperFundAmount)
	require.True(t, actualRune.GTE(math.Int(minExpectedRune)), fmt.Sprintf("Actual: %s, Min expected: %s", actualRune, minExpectedRune))
	require.True(t, actualRune.LTE(math.Int(maxExpectedRune)), fmt.Sprintf("Actual: %s, Max expected: %s", actualRune, maxExpectedRune))

	// --------------------------------------------------------
	// Saver Eject
	// --------------------------------------------------------
	// Reset mimirs
	mimirs, err = thorchain.ApiGetMimirs()
	require.NoError(t, err)

	if mimir, ok := mimirs["MaxSynthPerPoolDepth"]; (ok && mimir != int64(-1)) {
		err := thorchain.SetMimir(ctx, "admin", "MaxSynthPerPoolDepth", "-1")
		require.NoError(t, err)
	}

	if mimir, ok := mimirs["SaversEjectInterval"]; (ok && mimir != int64(-1)) {
		err := thorchain.SetMimir(ctx, "admin", "SaversEjectInterval", "-1")
		require.NoError(t, err)
	}

	saversEjectFundAmount := math.NewInt(100_000_000_000)
	users = interchaintest.GetAndFundTestUsers(t, ctx, "savers", saversEjectFundAmount, gaia)
	gaiaSaverEjectUser := users[0]

	pool, err = thorchain.ApiGetPool(common.ATOMAsset)
	require.NoError(t, err)
	saveEjectAmount := math.NewUintFromString(pool.BalanceAsset).
		MulUint64(2000).QuoUint64(10_000)

	saverEjectQuote, err := thorchain.ApiGetSaverDepositQuote(common.ATOMAsset, saveEjectAmount)
	require.NoError(t, err)
	
	// store expected range to fail if received amount is outside 5% tolerance
	saverEjectQuoteOut := math.NewUintFromString(saverEjectQuote.ExpectedAmountDeposit)
	toleranceEject := saverEjectQuoteOut.QuoUint64(20)
	if saverEjectQuote.Fees.Outbound != nil {
		outboundFee := math.NewUintFromString(*saverEjectQuote.Fees.Outbound)
		saverEjectQuoteOut = saverEjectQuoteOut.Add(outboundFee)
	}
	minExpectedSaverEject := saverEjectQuoteOut.Sub(toleranceEject)
	maxExpectedSaverEject := saverEjectQuoteOut.Add(toleranceEject)

	// Alternate between memo and memoless
	memo = fmt.Sprintf("+:%s", "GAIA/ATOM")
	//memo = ""
	gaiaInboundAddr, _, err = thorchain.ApiGetInboundAddress("GAIA")
	require.NoError(t, err)
	_, err = gaia.GetNode().BankSendWithMemo(ctx, gaiaSaverEjectUser.KeyName(), ibc.WalletAmount{
		Address: gaiaInboundAddr,
		Denom: gaia.Config().Denom,
		Amount: math.Int(saveEjectAmount).QuoRaw(100), // save amount is based on 8 dec
	}, memo)

	saverEjectUserFound := false
	for count := 0; !saverEjectUserFound; count++ {
		time.Sleep(time.Second)
		savers, err := thorchain.ApiGetSavers(common.ATOMAsset)
		require.NoError(t, err)
		for _, saver := range savers {
			if saver.AssetAddress != gaiaSaverEjectUser.FormattedAddress() {
				continue
			}
			saverEjectUserFound = true
			deposit := math.NewUintFromString(saver.AssetDepositValue)
			require.True(t, deposit.GTE(minExpectedSaverEject), fmt.Sprintf("Actual: %s, Min expected: %s", deposit, minExpectedSaverEject))
			require.True(t, deposit.LTE(maxExpectedSaverEject), fmt.Sprintf("Actual: %s, Max expected: %s", deposit, maxExpectedSaverEject))
		}
		require.Less(t, count, 30, "saver took longer than 30 sec to show")
	}

	gaiaSaverEjectUserBalance, err := gaia.GetBalance(ctx, gaiaSaverEjectUser.FormattedAddress(), gaia.Config().Denom)
	require.NoError(t, err)
	gaiaSaverBalance, err := gaia.GetBalance(ctx, gaiaSaver.FormattedAddress(), gaia.Config().Denom)
	require.NoError(t, err)

	// Set mimirs
	if mimir, ok := mimirs["MaxSynthPerPoolDepth"]; (ok && mimir != int64(500) || !ok) {
		err := thorchain.SetMimir(ctx, "admin", "MaxSynthPerPoolDepth", "500")
		require.NoError(t, err)
	}

	if mimir, ok := mimirs["SaversEjectInterval"]; (ok && mimir != int64(1) || !ok) {
		err := thorchain.SetMimir(ctx, "admin", "SaversEjectInterval", "1")
		require.NoError(t, err)
	}

	for count := 0; true; count++ {
		time.Sleep(time.Second)
		savers, err := thorchain.ApiGetSavers(common.ATOMAsset)
		require.NoError(t, err)
		saverEjectUserFound := false
		for _, saver := range savers {
			if saver.AssetAddress != gaiaSaverEjectUser.FormattedAddress() {
				continue
			}
			saverEjectUserFound = true
		}
		if !saverEjectUserFound {
			break
		}
		require.Less(t, count, 30, "saver took longer than 30 sec to show")
	}

	err = PollForBalanceChange(ctx, gaia, 15, ibc.WalletAmount{
		Address: gaiaSaverEjectUser.FormattedAddress(),
		Denom: gaia.Config().Denom,
		Amount: gaiaSaverEjectUserBalance,
	})
	gaiaSaverEjectUserAfterBalance, err := gaia.GetBalance(ctx, gaiaSaverEjectUser.FormattedAddress(), gaia.Config().Denom)
	require.NoError(t, err)
	require.True(t, gaiaSaverEjectUserAfterBalance.GT(gaiaSaverEjectUserBalance), fmt.Sprintf("Balance (%s) must be greater after ejection: %s", gaiaSaverEjectUserAfterBalance, gaiaSaverEjectUserBalance))
	gaiaSaverAfterBalance, err := gaia.GetBalance(ctx, gaiaSaver.FormattedAddress(), gaia.Config().Denom)
	require.NoError(t, err)
	require.True(t, gaiaSaverBalance.Equal(gaiaSaverAfterBalance), fmt.Sprintf("Balance (%s) should be the same (%s)", gaiaSaverAfterBalance, gaiaSaverBalance))

	// --------------------------------------------------------
	// Ragnarok gaia
	// --------------------------------------------------------
	pools, err = thorchain.ApiGetPools()
	require.NoError(t, err)
	require.Equal(t, 1, len(pools), "only 1 pool is expected")

	gaiaLperBalanceBeforeRag, err := gaia.GetBalance(ctx, gaiaLper.FormattedAddress(), gaia.Config().Denom)
	require.NoError(t, err)
	
	err = thorchain.SetMimir(ctx, "admin", "RAGNAROK-GAIA-ATOM", "1")
	require.NoError(t, err)

	pools, err = thorchain.ApiGetPools()
	require.NoError(t, err)
	count = 0
	for len(pools) > 0 {
		if pools[0].Status == "Suspended" {
			break
		}
		require.Less(t, count, 6, "atom pool didn't get torn down or suspended in 60 seconds")
		time.Sleep(10 * time.Second)
		pools, err = thorchain.ApiGetPools()
		require.NoError(t, err)
		count++
	}

	err = PollForBalanceChange(ctx, gaia, 100, ibc.WalletAmount{
		Address: gaiaLper.FormattedAddress(),
		Denom: gaia.Config().Denom,
		Amount: gaiaLperBalanceBeforeRag,
	})
	require.NoError(t, err)
	gaiaLperBalanceAfterRag, err := gaia.GetBalance(ctx, gaiaLper.FormattedAddress(), gaia.Config().Denom)
	require.NoError(t, err)
	require.True(t, gaiaLperBalanceAfterRag.GT(gaiaLperBalanceBeforeRag), fmt.Sprintf("Lper balance (%s) should be greater after ragnarok (%s)", gaiaLperBalanceAfterRag, gaiaLperBalanceBeforeRag))
	
	err = PollForBalanceChange(ctx, gaia, 30, ibc.WalletAmount{
		Address: gaiaSaver.FormattedAddress(),
		Denom: gaia.Config().Denom,
		Amount: gaiaSaverBalance,
	})
	require.NoError(t, err)
	gaiaSaverAfterBalance, err = gaia.GetBalance(ctx, gaiaSaver.FormattedAddress(), gaia.Config().Denom)
	require.NoError(t, err)
	require.True(t, gaiaSaverAfterBalance.GT(gaiaSaverBalance), fmt.Sprintf("Saver balance (%s) should be greater after ragnarok (%s)", gaiaSaverAfterBalance, gaiaSaverBalance))
	
	//err = gaia.StopAllNodes(ctx)
	//require.NoError(t, err)

	//state, err := gaia.ExportState(ctx, -1)
	//require.NoError(t, err)
	//fmt.Println("State: ", state)


	//err = testutil.WaitForBlocks(ctx, 300, thorchain)
	//require.NoError(t, err, "thorchain failed to make blocks")
}

func Min[T int | uint | int64 | uint64](a, b T) T {
	if a < b {
		return a
	}
	return b
}

func Abs[T int | int64](a T) T {
	if a < 0 {
		return -a
	}
	return a
}