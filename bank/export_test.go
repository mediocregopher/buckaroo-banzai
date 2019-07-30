package bank

import (
	"context"
	. "testing"
	"time"

	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mrand"
	"github.com/mediocregopher/mediocre-go-lib/mtest"
	"github.com/mediocregopher/mediocre-go-lib/mtest/massert"
)

func TestExportingBank(t *T) {
	cmp := mtest.Component()
	bank := Inst(cmp)
	userID := mrand.Hex(8)

	var initAmount = 9999
	var exportedAmount int

	exports := make([]Export, 3)
	for i := range exports {
		exports[i] = Export{
			FromUserID:      userID,
			Amount:          i + 1,
			Protocol:        mrand.Hex(8),
			ProtocolPayload: mrand.Hex(8),
		}
		exportedAmount += exports[i].Amount
	}

	mtest.Run(cmp, t, func() {
		bank.(*redisBank).keyPrefix = "test:bank-" + mrand.Hex(8)

		_, err := bank.Incr(userID, initAmount)
		massert.Require(t, massert.Nil(err))

		ch := make(chan ExportInProgress, len(exports))
		errCh := make(chan error, 1)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			errCh <- bank.ConsumeExports(ctx, ch)
			close(ch)
		}()

		ids := make([]string, len(exports))
		for i := range exports {
			ids[i], err = bank.SubmitExport(exports[i])
			massert.Require(t, massert.Nil(err))
		}

		var assertions []massert.Assertion
		gotExports := make([]ExportInProgress, len(exports))
		for i := range gotExports {
			select {
			case gotExports[i] = <-ch:
			case <-time.After(1 * time.Second):
				t.Fatal("timedout")
			}
			assertions = append(assertions,
				massert.Equal(ids[i], gotExports[i].ID),
				massert.Equal(exports[i], gotExports[i].Export))
		}
		massert.Require(t, assertions...)

		// check that acking appears to work
		assertions = assertions[:0]
		for i := range gotExports {
			assertions = append(assertions, massert.Nil(gotExports[i].Ack()))
		}
		massert.Require(t, assertions...)

		cancel()
		massert.Require(t, massert.Equal(context.Canceled, merr.Base(<-errCh)))

		balance, err := bank.Balance(userID)
		massert.Require(t,
			massert.Nil(err),
			massert.Equal(initAmount-exportedAmount, balance),
		)
	})
}
