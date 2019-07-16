// Package stellar contains generic functionality for interacting with stellar.
package stellar

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/stellar/go/clients/federation"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/strkey"
	"github.com/stellar/go/txnbuild"
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

// SendOpts describe the various options which can be sent into the Send method.
type SendOpts struct {
	From        *keypair.Full
	To          string // stellar or federation address
	Memo        string // may be overwritten if To is a federation addr
	AssetCode   string
	AssetIssuer string // required unless AssetCode is "XLM"
	Amount      string
}

// Send is used to send funds from one account to another. It will automatically
// resolve federated stellar addresses.
func (c *Client) Send(ctx context.Context, opts SendOpts) (horizon.TransactionSuccess, error) {
	var txRes horizon.TransactionSuccess

	ctx = mctx.Annotate(ctx, "sendFrom", opts.From.Address(), "sendTo", opts.To)
	if _, err := keypair.Parse(opts.To); err != nil {
		mlog.From(c.cmp).Info("resolving stellar federation address", ctx)
		res, err := c.FederationClient.LookupByAddress(opts.To)
		if err != nil {
			return txRes, merr.Wrap(err, c.cmp.Context(), ctx)
		}
		opts.To = res.AccountID
		ctx = mctx.Annotate(ctx, "sendTo", opts.To)
		if res.MemoType == "" {
			// ok
		} else if res.MemoType == "text" {
			opts.Memo = res.Memo.Value
		} else {
			return txRes, merr.New("unsupported memo type", c.cmp.Context(),
				mctx.Annotate(ctx, "memoType", res.MemoType))
		}
	}

	mlog.From(c.cmp).Info("retrieving source account", ctx)
	sourceAccount, err := c.AccountDetail(horizonclient.AccountRequest{
		AccountID: opts.From.Address(),
	})
	if err != nil {
		return txRes, merr.Wrap(err, c.cmp.Context(), ctx)
	}

	ctx = mctx.Annotate(ctx,
		"sendAssetCode", opts.AssetCode,
		"sendAmount", opts.Amount)
	if opts.Memo != "" {
		ctx = mctx.Annotate(ctx, "sendMemo", opts.Memo)
	}
	if opts.AssetIssuer != "" {
		ctx = mctx.Annotate(ctx, "sendAssetIssuer", opts.AssetIssuer)
	}

	asset := txnbuild.CreditAsset{
		Code:   opts.AssetCode,
		Issuer: opts.AssetIssuer,
	}

	op := txnbuild.Payment{
		Destination: opts.To,
		Amount:      opts.Amount,
		Asset:       asset,
	}

	timeout := txnbuild.NewInfiniteTimeout()
	if deadline, ok := ctx.Deadline(); ok {
		timeoutSeconds := int64(time.Until(deadline).Seconds())
		if timeoutSeconds > 0 {
			ctx = mctx.Annotate(ctx, "sendTimeout", timeoutSeconds)
			timeout = txnbuild.NewTimeout(timeoutSeconds)
		}
	}

	tx := txnbuild.Transaction{
		SourceAccount: &sourceAccount,
		Operations:    []txnbuild.Operation{&op},
		Timebounds:    timeout,
		Network:       c.NetworkPassphrase,
	}
	if opts.Memo != "" {
		tx.Memo = txnbuild.MemoText(opts.Memo)
	}

	txXDR, err := tx.BuildSignEncode(opts.From)
	if err != nil {
		return txRes, merr.Wrap(err, c.cmp.Context(), ctx)
	}

	mlog.From(c.cmp).Info("submitting transaction", ctx)
	if txRes, err = c.SubmitTransactionXDR(txXDR); err != nil {
		ctx = mctx.Annotate(ctx, "txXDR", txXDR)
		if herr, ok := err.(*horizonclient.Error); ok {
			b, _ := json.Marshal(herr.Problem)
			ctx = mctx.Annotate(ctx, "problem", string(b))
		}
		return txRes, merr.Wrap(err, c.cmp.Context(), ctx)
	}
	return txRes, nil
}

///////////////////////////////////////////////////////////////////////////////

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
