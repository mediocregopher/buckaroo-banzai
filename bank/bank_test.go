package bank

import (
	. "testing"

	"github.com/mediocregopher/mediocre-go-lib/mrand"
	"github.com/mediocregopher/mediocre-go-lib/mtest"
	"github.com/mediocregopher/mediocre-go-lib/mtest/massert"
)

func TestPersistentBank(t *T) {
	cmp := mtest.Component()
	bank := Inst(cmp)

	assertBalance := func(userID string, expBalance int) massert.Assertion {
		balance, err := bank.Balance(userID)
		return massert.All(
			massert.Nil(err),
			massert.Equal(expBalance, balance),
		)
	}

	assertIncr := func(userID string, by int, expBalance int) massert.Assertion {
		newBalance, err := bank.Incr(userID, by)
		if expBalance < 0 {
			return massert.Equal(true, IsNotEnoughFunds(err))
		}
		return massert.All(
			massert.Nil(err),
			massert.Equal(expBalance, newBalance),
		)
	}

	assertTransfer := func(to, from string, amount int, expDstBalance, expSrcBalance int) massert.Assertion {
		newDstBalance, newSrcBalance, err := bank.Transfer(to, from, amount)
		if expDstBalance < 0 || expSrcBalance < 0 {
			return massert.Equal(true, IsNotEnoughFunds(err))
		}
		return massert.All(
			massert.Nil(err),
			massert.Equal(expDstBalance, newDstBalance),
			massert.Equal(expSrcBalance, newSrcBalance),
		)
	}

	mtest.Run(cmp, t, func() {
		bank.(*redisBank).keyPrefix = "test:bank"
		userA, userB := mrand.Hex(8), mrand.Hex(8)

		massert.Require(t,
			assertBalance(userA, 0),
			assertBalance(userB, 0),

			// test incrementing
			assertIncr(userA, 1, 1),
			assertBalance(userA, 1),
			assertBalance(userB, 0),

			assertIncr(userA, 1, 2),
			assertBalance(userA, 2),
			assertBalance(userB, 0),

			assertIncr(userA, -1, 1),
			assertBalance(userA, 1),
			assertBalance(userB, 0),

			assertIncr(userA, -2, -1),
			assertBalance(userA, 1),
			assertBalance(userB, 0),

			assertIncr(userB, -1, -1),
			assertBalance(userA, 1),
			assertBalance(userB, 0),

			// test transfer
			assertTransfer(userA, userB, 1, -1, -1),
			assertBalance(userA, 1),
			assertBalance(userB, 0),

			assertTransfer(userB, userA, 2, -1, -1),
			assertBalance(userA, 1),
			assertBalance(userB, 0),

			assertTransfer(userB, userA, 1, 1, 0),
			assertBalance(userA, 0),
			assertBalance(userB, 1),
		)
	})
}
