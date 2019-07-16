// Package bank implements a db storage entity which handles accounting for each
// user's bucks.
package bank

import (
	"context"
	"strconv"

	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/mediocregopher/radix/v3"
)

var notEnoughFundsStr = "you aint got that kind of scratch, kid"

// IsNotEnoughFunds returns true if the given error is due to an account not
// having enough funds to perform some action.
func IsNotEnoughFunds(err error) bool {
	if err == nil {
		return false
	}
	return merr.Base(err).Error() == notEnoughFundsStr
}

// Bank describes a thread-safe store of user funds.
type Bank interface {
	// TODO the only reason Client is part of this interface is because Bank is
	// overloaded to also be the storage for stellar's lastCursor stuff. Once
	// instRedis gets moved to mediocre-go-lib we can have two redis insts, and
	// use one for last-cursor.
	radix.Client
	Balance(userID string) (int, error)
	Incr(userID string, by int) (newBalance int, err error)
	Transfer(dstUserID, srcUserID string, amount int) (newDstBalance, newSrcBalanc int, err error)
}

func instRedis(parent *mcmp.Component) radix.Client {
	cmp := parent.Child("redis")
	client := new(struct{ radix.Client })

	addr := mcfg.String(cmp, "addr",
		mcfg.ParamDefault("127.0.0.1:6379"),
		mcfg.ParamUsage("Address redis is listening on"))
	poolSize := mcfg.Int(cmp, "pool-size",
		mcfg.ParamDefault(10),
		mcfg.ParamUsage("Number of connections in pool"))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		cmp.Annotate("addr", *addr, "poolSize", *poolSize)
		mlog.From(cmp).Info("connecting to redis", ctx)
		var err error
		client.Client, err = radix.NewPool("tcp", *addr, *poolSize)
		return err
	})
	mrun.ShutdownHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("shutting down redis", ctx)
		return client.Close()
	})

	return client
}

type redisBank struct {
	cmp         *mcmp.Component
	balancesKey string
	radix.Client
}

// Inst instantiates a Bank which will be configured and initialized when the
// Init hook is run.
func Inst(parent *mcmp.Component) Bank {
	cmp := parent.Child("bank")
	return &redisBank{
		cmp:         cmp,
		balancesKey: "balances",
		Client:      instRedis(cmp),
	}
}

func (b *redisBank) Balance(userID string) (int, error) {
	var amount int
	err := b.Do(radix.Cmd(&amount, "HGET", b.balancesKey, userID))
	return amount, merr.Wrap(err, b.cmp.Context())
}

// Keys:[balancesKey] Args:[user, amount]
var incrCmd = radix.NewEvalScript(1, `
	local toIncr = tonumber(ARGV[2])
	local balance = tonumber(redis.call("HGET", KEYS[1], ARGV[1]))
	if not balance then balance = 0 end
	if balance + toIncr < 0 then
		return redis.error_reply("`+notEnoughFundsStr+`")
	end

	return redis.call("HINCRBY", KEYS[1], ARGV[1], toIncr)
`)

func (b *redisBank) Incr(userID string, by int) (int, error) {
	var newBalance int
	err := b.Do(incrCmd.Cmd(&newBalance, b.balancesKey, userID, strconv.Itoa(by)))
	return newBalance, merr.Wrap(err, b.cmp.Context())
}

// Keys:[balancesKey] Args:[dstUser, srcUser, amount]
var transferCmd = radix.NewEvalScript(1, `
	local toTransfer = tonumber(ARGV[3])
	local srcBalance = tonumber(redis.call("HGET", KEYS[1], ARGV[2]))
	if not srcBalance then srcBalance = 0 end
	if srcBalance < toTransfer then
		return redis.error_reply("`+notEnoughFundsStr+`")
	end

	local newDstBalance = redis.call("HINCRBY", KEYS[1], ARGV[1], toTransfer)
	local newSrcBalance = redis.call("HINCRBY", KEYS[1], ARGV[2], -1*toTransfer)
	return {newDstBalance, newSrcBalance}
`)

func (b *redisBank) Transfer(dstUserID, srcUserID string, amount int) (int, int, error) {
	var newBalances []int
	err := b.Do(transferCmd.Cmd(
		&newBalances, b.balancesKey, dstUserID, srcUserID, strconv.Itoa(amount),
	))
	if err != nil {
		return 0, 0, merr.Wrap(err, b.cmp.Context())
	}
	return newBalances[0], newBalances[1], nil
}
