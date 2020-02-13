// Package stellar-cli is a collection of CLI utilities for working with stellar
// from the command-line. It is effectively a stellar wallet.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mediocregopher/mediocre-go-lib/m"
	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/txnbuild"

	"buckaroo-banzai/stellar"
)

func jsonDump(v interface{}) {
	b, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		panic(fmt.Sprintf("couldn't json marshal %#v: %v", v, err))
	}
	fmt.Println(string(b))
}

func cmdGen(cmp *mcmp.Component) {
	mrun.InitHook(cmp, func(ctx context.Context) error {
		pair, err := keypair.Random()
		if err != nil {
			return fmt.Errorf("error generating random keypair: %w", err)
		}

		mlog.From(cmp).Info("keypair generated", mctx.Annotate(ctx,
			"address", pair.Address(),
			"seed", pair.Seed(),
		))
		return nil
	})
}

func cmdDump(cmp *mcmp.Component) {
	client := stellar.InstClient(cmp, false)
	seed := mcfg.String(cmp, "seed",
		mcfg.ParamUsage("Seed to dump all information about (mutually exclusive with -addr)"))
	addr := mcfg.String(cmp, "addr",
		mcfg.ParamUsage("Addr to dump all information about (mutually exclusive with -seed)"))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		if (*seed == "" && *addr == "") || (*seed != "" && *addr != "") {
			return errors.New("Exactly one of --seed and --addr should be given")
		}

		var accountReq horizonclient.AccountRequest
		if *seed != "" {
			pair, err := stellar.LoadKeyPair(*seed)
			if err != nil {
				return fmt.Errorf("error loading keypair from seed (REDACTED): %w", err)
			}
			accountReq.AccountID = pair.Address()
		} else {
			accountReq.AccountID = *addr
		}
		ctx = mctx.Annotate(ctx, "addr", accountReq.AccountID)

		mlog.From(cmp).Info("loading account details", ctx)
		detail, err := client.AccountDetail(accountReq)
		if err != nil {
			return fmt.Errorf("error loading account details with req %+v: %w",
				accountReq, stellar.HorizonErr(err))
		}

		jsonDump(detail)
		return nil
	})
}

func cmdFund(cmp *mcmp.Component) {
	client := stellar.InstClient(cmp, false)
	addr := mcfg.String(cmp, "addr",
		mcfg.ParamUsage("Addr to fund"))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		ctx = mctx.Annotate(ctx, "addr", *addr)
		mlog.From(cmp).Info("funding account", ctx)
		res, err := client.Fund(*addr)
		if err != nil {
			return fmt.Errorf("error funding account %q: %w",
				*addr, stellar.HorizonErr(err))
		}
		jsonDump(res)
		return nil
	})
}

func cmdResolve(cmp *mcmp.Component) {
	client := stellar.InstClient(cmp, false)
	name := mcfg.String(cmp, "name",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Name to resolve."))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		res, err := client.FederationClient.LookupByAddress(*name)
		if err != nil {
			return fmt.Errorf("error looking up address for %q: %w", *name, err)
		}
		jsonDump(res)
		return nil
	})
}

func cmdTrust(cmp *mcmp.Component) {
	client := stellar.InstClient(cmp, false)
	pair := stellar.InstKeyPair(cmp)
	assetCode := mcfg.String(cmp, "asset-code",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Asset code to issue trust for"))
	assetIssuer := mcfg.String(cmp, "asset-issuer",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Issuing address of the asset to trust"))
	limit := mcfg.Int(cmp, "limit",
		mcfg.ParamDefault(999999),
		mcfg.ParamUsage("Limit of the asset to trust"))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		sourceAccount, err := client.AccountDetail(horizonclient.AccountRequest{
			AccountID: pair.Address(),
		})
		if err != nil {
			return fmt.Errorf("error getting account detail of %q: %w",
				pair.Address(), stellar.HorizonErr(err))
		}

		ctx = mctx.Annotate(ctx, "assetCode", *assetCode)

		op := txnbuild.ChangeTrust{
			Line: txnbuild.CreditAsset{
				Code:   *assetCode,
				Issuer: *assetIssuer,
			},
			Limit: strconv.Itoa(*limit),
		}

		tx := txnbuild.Transaction{
			SourceAccount: &sourceAccount,
			Operations:    []txnbuild.Operation{&op},
			Timebounds:    txnbuild.NewInfiniteTimeout(),
			Network:       client.NetworkPassphrase,
		}

		txXDR, err := tx.BuildSignEncode(pair)
		if err != nil {
			return fmt.Errorf("error calling BuildSignEncode: %w", err)
		}

		txRes, err := client.SubmitTransactionXDR(ctx, txXDR)
		jsonDump(txRes)
		if err != nil {
			return fmt.Errorf("error submitting transaction %q: %w",
				txXDR, stellar.HorizonErr(err))
		}
		return nil
	})
}

func cmdSend(cmp *mcmp.Component) {
	client := stellar.InstClient(cmp, false)
	pair := stellar.InstKeyPair(cmp)
	assetCode := mcfg.String(cmp, "asset-code",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Asset code to send"))
	assetIssuer := mcfg.String(cmp, "asset-issuer",
		mcfg.ParamUsage("Issuing address of the asset to send, if it's a token"))
	amount := mcfg.String(cmp, "amount",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Amount of the asset to send"))
	dstAddress := mcfg.String(cmp, "dst",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Address to send to."))
	memo := mcfg.String(cmp, "memo",
		mcfg.ParamUsage("Memo to attach to transaction. If --dst is a federated stellar address then this memo might get overwritten."))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		if strings.ToUpper(*assetCode) != "XLM" && *assetIssuer == "" {
			return errors.New("asset-issuer required for non-native asset")
		}

		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		txRes, err := client.Send(ctx, stellar.SendOpts{
			From:        pair,
			To:          *dstAddress,
			Memo:        *memo,
			AssetCode:   *assetCode,
			AssetIssuer: *assetIssuer,
			Amount:      *amount,
		})
		cancel()

		if err != nil {
			return fmt.Errorf("error sending: %w", stellar.HorizonErr(err))
		}
		jsonDump(txRes)
		return nil
	})
}

func main() {
	cmp := m.RootComponent()
	mcfg.CLISubCommand(cmp, "gen", "Generate a new stellar seed and address", cmdGen)
	mcfg.CLISubCommand(cmp, "dump", "Dump all information about an account", cmdDump)
	mcfg.CLISubCommand(cmp, "fund", "Funds an account with some funds (only works on test net)", cmdFund)
	mcfg.CLISubCommand(cmp, "resolve", "Resolve a name via the federation protocol", cmdResolve)
	mcfg.CLISubCommand(cmp, "trust", "Add a trust line", cmdTrust)
	mcfg.CLISubCommand(cmp, "send", "Send an asset to another account", cmdSend)

	m.MustInit(cmp)
	os.Stdout.Sync()
	os.Stderr.Sync()
}
