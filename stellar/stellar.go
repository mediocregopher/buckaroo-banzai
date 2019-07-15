// Package stellar contains generic functionality for interacting with stellar.
package stellar

import (
	"context"

	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/stellar/go/clients/federation"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/strkey"
)

// Client wraps a horizon client for stellar.
type Client struct {
	cmp *mcmp.Component
	*horizonclient.Client
	FederationClient  *federation.Client
	NetworkPassphrase string
}

// InstClient instantiates a Client which will be intialized and configured by
// mrun's Init hook.
//
// If child is false then this will be instantiated on the given Component,
// otherwise it will be instantiated on a child Component called "stellar".
func InstClient(parent *mcmp.Component, child bool) *Client {
	client := &Client{
		cmp: parent,
	}
	if child {
		client.cmp = client.cmp.Child("stellar")
	}

	live := mcfg.Bool(client.cmp, "live-net",
		mcfg.ParamUsage("Use the live network."))
	mrun.InitHook(client.cmp, func(ctx context.Context) error {
		if *live {
			mlog.From(client.cmp).Warn("connecting to live net", ctx)
			client.Client = horizonclient.DefaultPublicNetClient
			client.FederationClient = federation.DefaultPublicNetClient
			client.NetworkPassphrase = network.PublicNetworkPassphrase
		} else {
			mlog.From(client.cmp).Info("connecting to test net", ctx)
			client.Client = horizonclient.DefaultTestNetClient
			client.FederationClient = federation.DefaultTestNetClient
			client.NetworkPassphrase = network.TestNetworkPassphrase
		}
		return nil
	})
	return client
}

// LoadKeyPair takes a seed string and returns the full keypair object for it.
func LoadKeyPair(seed string) (*keypair.Full, error) {
	seedB, err := strkey.Decode(strkey.VersionByteSeed, seed)
	if err != nil {
		return nil, merr.Wrap(err)
	} else if len(seedB) != 32 {
		return nil, merr.New("invalid seed string")
	}
	var seedB32 [32]byte
	copy(seedB32[:], seedB)
	pair, err := keypair.FromRawSeed(seedB32)
	return pair, merr.Wrap(err)
}

// InstKeyPair instantiates a keypair onto the given Component. The keypair will
// be initialized and configured by mrun's Init hook.
func InstKeyPair(cmp *mcmp.Component) *keypair.Full {
	kp := new(keypair.Full)

	seedStr := mcfg.String(cmp, "seed",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Seed for account which will issue CRYPTICBUCKs"))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		pair, err := LoadKeyPair(*seedStr)
		if err != nil {
			return merr.Wrap(err, cmp.Context(), ctx)
		}
		*kp = *pair

		cmp.Annotate("address", kp.Address())
		mlog.From(cmp).Info("loaded stellar seed", ctx)
		return nil
	})

	return kp
}
