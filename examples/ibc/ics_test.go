package ibc_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	interchaintest "github.com/strangelove-ventures/interchaintest/v7"
	"github.com/strangelove-ventures/interchaintest/v7/ibc"
	"github.com/strangelove-ventures/interchaintest/v7/testreporter"
	"github.com/strangelove-ventures/interchaintest/v7/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// This tests Cosmos Interchain Security, spinning up a provider and a single consumer chain.
func TestICS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	relayers := []relayerImp{
		{
			name:       "Cosmos Relayer",
			relayerImp: ibc.CosmosRly,
		},
		{
			name:       "Hermes",
			relayerImp: ibc.Hermes,
		},
	}

	icsVersions := []string{"v3.1.0", "v3.3.0", "v4.0.0"}

	for _, rly := range relayers {
		rly := rly
		testname := rly.name
		t.Run(testname, func(t *testing.T) {
			// We paralellize the relayers, but not the versions. That would be too many tests running at once, and things can become unstable.
			t.Parallel()
			for _, providerVersion := range icsVersions {
				providerVersion := providerVersion
				for _, consumerVersion := range icsVersions {
					consumerVersion := consumerVersion
					testname := fmt.Sprintf("provider%s-consumer%s", providerVersion, consumerVersion)
					testname = strings.ReplaceAll(testname, ".", "")
					t.Run(testname, func(t *testing.T) {
						fullNodes := 0
						validators := 1
						ctx := context.Background()
						var consumerBechPrefix string
						if consumerVersion == "v4.0.0" {
							consumerBechPrefix = "consumer"
						}

						// Chain Factory
						cf := interchaintest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*interchaintest.ChainSpec{
							{Name: "ics-provider", Version: providerVersion, NumValidators: &validators, NumFullNodes: &fullNodes, ChainConfig: ibc.ChainConfig{GasAdjustment: 1.5}},
							{Name: "ics-consumer", Version: consumerVersion, NumValidators: &validators, NumFullNodes: &fullNodes, ChainConfig: ibc.ChainConfig{Bech32Prefix: consumerBechPrefix}},
						})

						chains, err := cf.Chains(t.Name())
						require.NoError(t, err)
						provider, consumer := chains[0], chains[1]

						// Relayer Factory
						client, network := interchaintest.DockerSetup(t)

						r := interchaintest.NewBuiltinRelayerFactory(
							rly.relayerImp,
							zaptest.NewLogger(t),
						).Build(t, client, network)

						// Prep Interchain
						const ibcPath = "ics-path"
						ic := interchaintest.NewInterchain().
							AddChain(provider).
							AddChain(consumer).
							AddRelayer(r, "relayer").
							AddProviderConsumerLink(interchaintest.ProviderConsumerLink{
								Provider: provider,
								Consumer: consumer,
								Relayer:  r,
								Path:     ibcPath,
							})

						// Log location
						f, err := interchaintest.CreateLogFile(fmt.Sprintf("%d.json", time.Now().Unix()))
						require.NoError(t, err)
						// Reporter/logs
						rep := testreporter.NewReporter(f)
						eRep := rep.RelayerExecReporter(t)

						// Build interchain
						err = ic.Build(ctx, eRep, interchaintest.InterchainBuildOptions{
							TestName:  t.Name(),
							Client:    client,
							NetworkID: network,

							SkipPathCreation: false,
						})
						require.NoError(t, err, "failed to build interchain")

						err = testutil.WaitForBlocks(ctx, 10, provider, consumer)
						require.NoError(t, err, "failed to wait for blocks")
					})
				}
			}
		})
	}
}
