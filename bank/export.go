package bank

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/mdb/mredis"
	"github.com/mediocregopher/radix/v3"
)

// Export wraps all information needed to execute a transfer of funds from a
// user's account to some system outside of the Bank (e.g. a crypto chain).
type Export struct {
	FromUserID string
	Amount     int

	// The protocol the funds are being transferred to.
	Protocol string

	// ProtocolPayload contains whatever data is required to transfer the funds
	// to a particular protocol (e.g. a crypto chain). It's content is specific
	// to the protocol.
	ProtocolPayload string
}

// Annotate returns the given Context annotated with information about the
// Export.
func (e Export) Annotate(ctx context.Context) context.Context {
	return mctx.Annotate(ctx,
		"exportFromUserID", e.FromUserID,
		"exportAmount", e.Amount,
		"exportProtocol", e.Protocol,
	)
}

// ExportInProgress describes an Export which has yet to be successfully
// consumed.
type ExportInProgress struct {
	ID string
	Export

	// Ack and Nack are used to declare that consuming an Export has been a
	// success or a failure, respectively. If Ack'd the Export will not be
	// consumed again, if Nack'd it will.
	//
	// In the event that an Ack fails it should be expected that it will be
	// retried again. This means that _all_ aspects of consuming an Export
	// should be idempotent.
	Ack, Nack func() error
}

// Annotate returns the given Context annotated with information about the
// ExportInProgress.
func (ep ExportInProgress) Annotate(ctx context.Context) context.Context {
	ctx = ep.Export.Annotate(ctx)
	return mctx.Annotate(ctx, "exportID", ep.ID)
}

// ExportingBank describes a Bank which is capable of enabling Export actions.
type ExportingBank interface {
	Bank

	// SubmitExport records that an Export is desired and returns a unique
	// identifier for it. All submitted Exports will be made available via
	// ConsumeExports at least once.
	SubmitExport(Export) (string, error)

	// ConsumeExports writes submitted Exports into the given channel. If
	// multiple ConsumeExports run at the same time then submitted Exports will
	// be divided between them.
	//
	// This method will block internally while writing to the channel, so be
	// sure to always be reading from it.
	//
	// This method will return when either the given Context is canceled or some
	// other error is encountered. Either way it will never return nil, and does
	// not close the given channel. It can be re-called if an error is returned.
	ConsumeExports(context.Context, chan<- ExportInProgress) error
}

///////////////////////////////////////////////////////////////////////////////

func (b *redisBank) exportsKey() string {
	return b.key("exports")
}

// Keys:[balancesKey, streamKey] Args:[user, amount, exportJSON]
var submitExportCmd = radix.NewEvalScript(2, `
	local toTransfer = tonumber(ARGV[2])
	local srcBalance = tonumber(redis.call("HGET", KEYS[1], ARGV[1]))
	if not srcBalance then srcBalance = 0 end
	if srcBalance < toTransfer then
		return redis.error_reply("`+ErrNotEnoughFunds.Error()+`")
	end

	redis.call("HINCRBY", KEYS[1], ARGV[1], -1*toTransfer)
	return redis.call("XADD", KEYS[2], "*", "json", ARGV[3])
`)

func (b *redisBank) SubmitExport(e Export) (string, error) {
	if e.Amount <= 0 {
		return "", fmt.Errorf("malformed Export.Amount: %d", e.Amount)
	}

	exportJSON, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("could not marshal Export %+v: %w", e, err)
	}

	var id radix.StreamEntryID
	err = b.Do(submitExportCmd.Cmd(
		&id, b.balancesKey(), b.exportsKey(), e.FromUserID,
		strconv.Itoa(e.Amount), string(exportJSON),
	))
	err = translateRedisErr(err)
	if err != nil {
		return "", fmt.Errorf("error performing export command in redis: %w", err)
	}
	return id.String(), nil
}

func (b *redisBank) ConsumeExports(ctx context.Context, ch chan<- ExportInProgress) error {
	key := b.exportsKey()
	group := "redisBank.ConsumeExports"

	reader := mredis.NewStream(b.Redis, mredis.StreamOpts{
		Key:           key,
		Group:         group,
		Consumer:      b.instanceID,
		Block:         redisBankReadTimeout / 2,
		InitialCursor: "0",
	})

	for {
		if err := ctx.Err(); err != nil {
			return ctx.Err()
		}

		entry, ok, err := reader.Next()
		if err != nil {
			return fmt.Errorf("error consuming next Export from stream: %w", err)
		} else if !ok {
			continue
		}

		exportJSONStr := entry.Fields["json"]
		var export Export
		if err := json.Unmarshal([]byte(exportJSONStr), &export); err != nil {
			return fmt.Errorf("error unmarshaling Export %q: %w", exportJSONStr, err)
		}

		id := entry.ID.String()
		ch <- ExportInProgress{
			ID:     id,
			Export: export,
			Ack:    entry.Ack,
			Nack: func() error {
				entry.Nack()
				return nil
			},
		}
	}
}
