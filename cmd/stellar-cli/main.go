// Package stellar-cli is a collection of CLI utilities for working with stellar
// from the command-line. It is effectively a stellar wallet.
package main

import (
	"context"
	"os"

	"github.com/mediocregopher/mediocre-go-lib/m"
	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/stellar/go/keypair"
)

func main() {
	cmp := m.RootComponent()
	mcfg.CLISubCommand(cmp, "gen", "Generate a new stellar seed and address",
		func(cmp *mcmp.Component) {
			mrun.InitHook(cmp, func(ctx context.Context) error {
				pair, err := keypair.Random()
				if err != nil {
					return merr.Wrap(err, cmp.Context(), ctx)
				}

				mlog.From(cmp).Info("keypair generated", mctx.Annotate(ctx,
					"address", pair.Address(),
					"seed", pair.Seed(),
				))
				return nil
			})
		})

	//stellar.InstClient(cmp, false)
	//stellar.InstKeyPair(cmp)

	m.MustInit(cmp)
	os.Stdout.Sync()
	os.Stderr.Sync()
}
