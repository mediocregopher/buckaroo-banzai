// Package stellar contains generic functionality for interacting with stellar.
package stellar

import (
	"context"
	"encoding/json"
	"fmt"
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

// FormatErr takes in an error returned by the stellar client, unpacks its
// internal fields, and returns an error which is formatted to be more useful.
//
// If the error was not one returned by the stellar client it is returned as-is.
func FormatErr(err error) error {
	herr, ok := err.(*horizonclient.Error)
	if !ok {
		return err
	}

	b, _ := json.Marshal(herr.Problem)
	return fmt.Errorf("horizon ERR: %q - %s", herr.Problem.Title, b)
}

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

// ResolveAddr takes in either a stellar address or a federated stellar address,
// and returns a stellar address and a memo.
//
// If a stellar address is given then it is returned directly with an empty
// memo.
//
// If a federated stellar address is given then it is resolved and the
// associated address/memo are returned.
func (c *Client) ResolveAddr(ctx context.Context, addr string) (string, string, error) {
	if _, err := keypair.Parse(addr); err == nil {
		return addr, "", nil
	}

	ctx = mctx.Annotate(ctx, "federatedAddr", addr)
	mlog.From(c.cmp).Info("resolving stellar federation address", ctx)
	res, err := c.FederationClient.LookupByAddress(addr)
	if err != nil {
		return "", "", merr.Wrap(err, c.cmp.Context(), ctx)
	}
	addr = res.AccountID
	ctx = mctx.Annotate(ctx, "addr", addr)

	var memo string
	if res.MemoType == "" {
		// ok
	} else if res.MemoType == "text" {
		memo = res.Memo.Value
	} else {
		return "", "", merr.New("unsupported memo type", c.cmp.Context(),
			mctx.Annotate(ctx, "memoType", res.MemoType))
	}

	return addr, memo, nil
}

// TransactionResult is returned from SubmitTransactionXDR and other methods
// which submit a transaction to the stellar network.
type TransactionResult = horizon.TransactionSuccess

// SubmitTransactionXDR attempts to submit the given XDR encoded transaction to
// the stellar network.
func (c *Client) SubmitTransactionXDR(ctx context.Context, txXDR string) (TransactionResult, error) {
	ctx = mctx.Annotate(ctx, "txXDR", txXDR)
	mlog.From(c.cmp).Info("submitting transaction", ctx)
	txRes, err := c.Client.SubmitTransactionXDR(txXDR)
	if err == nil {
		return txRes, err
	}

	return txRes, merr.Wrap(FormatErr(err), c.cmp.Context(), ctx)
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

func (opts SendOpts) annotate(ctx context.Context) context.Context {
	ctx = mctx.Annotate(ctx,
		"sendFrom", opts.From.Address(),
		"sendTo", opts.To,
		"sendAssetCode", opts.AssetCode,
		"sendAmount", opts.Amount,
	)
	if opts.Memo != "" {
		ctx = mctx.Annotate(ctx, "sendMemo", opts.Memo)
	}
	if opts.AssetIssuer != "" {
		ctx = mctx.Annotate(ctx, "sendAssetIssuer", opts.AssetIssuer)
	}
	return ctx
}

// MakeSendXDR constructs a transaction which sends funds according to the given
// SendOpts, and returns the XDR encoding of that transaction without submitting
// it to the stellar network.
func (c *Client) MakeSendXDR(ctx context.Context, opts SendOpts) (string, error) {
	addr, memo, err := c.ResolveAddr(ctx, opts.To)
	if err != nil {
		return "", merr.Wrap(err, c.cmp.Context(), ctx)
	}
	opts.To = addr
	if memo != "" {
		opts.Memo = memo
	}
	ctx = opts.annotate(ctx)

	mlog.From(c.cmp).Info("retrieving source account", ctx)
	sourceAccount, err := c.AccountDetail(horizonclient.AccountRequest{
		AccountID: opts.From.Address(),
	})
	if err != nil {
		return "", merr.Wrap(err, c.cmp.Context(), ctx)
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
		return "", merr.Wrap(err, c.cmp.Context(), ctx)
	}
	return txXDR, nil
}

// Send is used to send funds from one account to another. It will automatically
// resolve federated stellar addresses.
func (c *Client) Send(ctx context.Context, opts SendOpts) (TransactionResult, error) {
	txXDR, err := c.MakeSendXDR(ctx, opts)
	if err != nil {
		return TransactionResult{}, merr.Wrap(err, c.cmp.Context(), ctx)
	}

	return c.SubmitTransactionXDR(ctx, txXDR)
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
		mcfg.ParamUsage("Seed for account which will issue tokens"))
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
