package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mediocregopher/mediocre-go-lib/m"
	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/mediocregopher/radix/v3"
	"github.com/nlopes/slack"
	"github.com/stellar/go/protocols/horizon/operations"
)

var gitRef string

type slackClient struct {
	Client *slack.Client
	RTM    *slack.RTM
}

func instSlackClient(parent *mcmp.Component) *slackClient {
	cmp := parent.Child("slack")
	client := new(slackClient)

	token := mcfg.String(cmp, "token",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("API token for the buckaroo bonzai bot"))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("connecting to slack", ctx)
		client.Client = slack.New(*token)
		client.RTM = client.Client.NewRTM()
		go client.RTM.ManageConnection()
		return nil
	})
	mrun.ShutdownHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("shutting down slack", ctx)
		return client.RTM.Disconnect()
	})

	return client
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

type app struct {
	cmp                *mcmp.Component
	botUserID, botUser string

	channels    map[string]*slack.Channel
	users       map[string]*slack.User
	usersByName map[string]*slack.User

	slackClient *slackClient
	redis       radix.Client
	stellar     *stellarServer

	// if true then buckaroo won't speak or listen to anyone speaking to him.
	ghost bool
}

const balancesKey = "balances"

func (a *app) helpMsg() string {
	return strings.TrimSpace(fmt.Sprintf("Hi, I'm Buckaroo Bonzai, the sole purveyor of CRYPTICBUCKs! You gain one CRYPTICBUCK whenever someone adds a reaction to one of your messages, and by talking to me you can give them to other people in the slack team, or even export them as Stellar tokens! @ me or DM me with any of the following commands:\n\n```"+`
@%s balance                         // I will respond with your balance
@%s give <user> <amount>            // Give CRYPTICBUCKs to <user> (how nice!)
@%s send <stellar address> <amount> // Send CRYPTICBUCKs to <stellar address>
`+"```\nNOTE that you must have a trustline established to ??? for the token CRYPTICBUCKs to use the export command",
		a.botUser, a.botUser, a.botUser,
	))
}

func (a *app) getChannel(id string) (*slack.Channel, error) {
	if channel, ok := a.channels[id]; ok {
		return channel, nil
	}
	channel, err := a.slackClient.Client.GetConversationInfo(id, true)
	if err == nil {
		a.channels[id] = channel
	}
	return channel, err
}

func (a *app) getUser(id string) (*slack.User, error) {
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimPrefix(id, "@")
	id = strings.TrimSuffix(id, ">")
	if user, ok := a.users[id]; ok {
		return user, nil
	}

	user, err := a.slackClient.Client.GetUserInfo(id)
	if err == nil {
		a.users[id] = user
	}
	return user, err
}

func (a *app) refreshUsersByName() error {
	users, err := a.slackClient.Client.GetUsers()
	if err != nil {
		return merr.Wrap(err, a.cmp.Context())
	}
	a.usersByName = make(map[string]*slack.User, len(users))
	for i, user := range users {
		a.usersByName[user.Name] = &users[i]
	}
	return nil
}

func (a *app) getUserByName(name string) (*slack.User, error) {
	user, ok := a.usersByName[name]
	if ok {
		return user, nil
	} else if err := a.refreshUsersByName(); err != nil {
		return nil, merr.Wrap(err, a.cmp.Context())
	}
	user, ok = a.usersByName[name]
	if !ok {
		return nil, merr.New("user not found",
			mctx.Annotate(a.cmp.Context(), "user", user))
	}
	return user, nil
}

const errNotEnoughBucks = "you aint got that kind of scratch"

// Keys:[balancesKey] Args:[dstUser, srcUser, amount]
var giveCmd = radix.NewEvalScript(1, `
	local amount = tonumber(ARGV[3])
	local srcAmount = tonumber(redis.call("HGET", KEYS[1], ARGV[2]))
	if not srcAmount or (srcAmount < amount) then
		return redis.error_reply("`+errNotEnoughBucks+`")
	end

	redis.call("HINCRBY", KEYS[1], ARGV[1], amount)
	redis.call("HINCRBY", KEYS[1], ARGV[2], -1*amount)
	return "OK"
`)

func (a *app) processSlackMsg(ctx context.Context, channelID, userID, msg string) error {
	ctx = mctx.Annotate(ctx, "channelID", channelID)
	channel, err := a.getChannel(channelID)
	if err != nil {
		return merr.Wrap(err, ctx)
	}
	ctx = mctx.Annotate(ctx, "userID", userID, "channel", channel.Name)

	user, err := a.getUser(userID)
	if err != nil {
		return merr.Wrap(err, ctx)
	}
	ctx = mctx.Annotate(ctx, "user", user.Name)

	msg = strings.TrimSpace(msg)
	prefix := "<@" + a.botUserID + ">"
	if !strings.HasPrefix(msg, prefix) && !channel.IsIM {
		return nil
	}
	msg = strings.TrimPrefix(msg, prefix)
	fields := strings.Fields(msg)

	sendMsg := func(channelID, str string, args ...interface{}) {
		str = fmt.Sprintf(str, args...)
		if !channel.IsIM {
			str = fmt.Sprintf("<@%s> %s", userID, str)
		}
		outMsg := a.slackClient.RTM.NewOutgoingMessage(str, channelID)
		a.slackClient.RTM.SendMessage(outMsg)
	}

	if len(fields) < 1 {
		sendMsg(channelID, a.helpMsg())
		return nil
	}

	var outErr error
	switch strings.ToLower(fields[0]) {
	case "ref":
		sendMsg(channelID, "Current git ref is `%s`", gitRef)
	case "balance":
		ctx = mctx.Annotate(ctx, "command", "balance")
		mlog.From(a.cmp).Info("getting user balance", ctx)
		var amount int
		if err := a.redis.Do(radix.Cmd(&amount, "HGET", balancesKey, userID)); err != nil {
			outErr = err
			break
		}
		if amount == 0 {
			sendMsg(channelID, "sorry champ, you don't have any CRYPTICBUCKs :( if you're having trouble getting CRYPTICBUCKs, try being cool!")
		} else if amount == 1 {
			sendMsg(channelID, "you have 1 CRYPTICBUCK!")
		} else if amount < 0 {
			sendMsg(channelID, "you have %d CRYPTICBUCKs! that's not even possible :face_with_monocle:", amount)
		} else {
			sendMsg(channelID, "you have %d CRYPTICBUCKs!", amount)
		}

	case "give":
		if len(fields) != 3 {
			break
		}
		ctx = mctx.Annotate(ctx, "command", "give", "dstUserID", fields[1])
		dstUser, err := a.getUser(fields[1])
		if err != nil {
			outErr = err
			break
		}
		ctx = mctx.Annotate(ctx, "dstUser", dstUser.Name, "dstUserID", dstUser.ID)

		if dstUser.ID == userID {
			sendMsg(channelID, "quit playing with yourself, kid")
			break
		}

		ctx = mctx.Annotate(ctx, "amount", fields[2])
		amount, err := strconv.Atoi(fields[2])
		if err != nil {
			outErr = err
			break
		}

		mlog.From(a.cmp).Info("giving bucks", ctx)
		if err = a.redis.Do(giveCmd.Cmd(
			nil, balancesKey, dstUser.ID, user.ID, strconv.Itoa(amount),
		)); err != nil {
			outErr = err
			break
		}

		sendMsg(channelID, "you gave <@%s> %d CRYPTICBUCK(s) :money_with_wings:", dstUser.ID, amount)

		// don't dm a bot, it errors out
		if dstUser.IsBot {
			break
		}

		_, _, imChannelID, err := a.slackClient.Client.OpenIMChannel(dstUser.ID)
		if err != nil {
			outErr = err
			break
		}
		sendMsg(imChannelID, "your friend <@%s> gave you %d CRYPTICBUCKs! You can reply to this message with `help` if you don't know what that means :)", userID, amount)

	default:
		sendMsg(channelID, a.helpMsg())
		return nil
	}

	if outErr != nil {
		sendMsg(channelID, "error: %s", outErr)
		return merr.Wrap(outErr, ctx)
	}

	return nil
}

func (a *app) processSlackEvent(e slack.RTMEvent) {
	ctx := context.Background()
	//{
	//	b, err := json.MarshalIndent(e, "", "  ")
	//	if err != nil {
	//		panic(err)
	//	}
	//	fmt.Printf("got message: %s\n", string(b))
	//}

	switch e.Type {
	case "reaction_added":
		data, ok := e.Data.(*slack.ReactionAddedEvent)
		if !ok || data.User == data.ItemUser {
			return
		}
		ctx = mctx.Annotate(ctx, "user", data.ItemUser)
		mlog.From(a.cmp).Info("incrementing user's balance", ctx)
		if err := a.redis.Do(radix.Cmd(nil, "HINCRBY", balancesKey, data.ItemUser, "1")); err != nil {
			mlog.From(a.cmp).Error("error incrementing user's balance", ctx, merr.Context(err))
		}
	case "reaction_removed":
		data, ok := e.Data.(*slack.ReactionRemovedEvent)
		if !ok || data.User == data.ItemUser {
			return
		}
		ctx = mctx.Annotate(ctx, "user", data.ItemUser)
		mlog.From(a.cmp).Info("decrementing user's balance", ctx)
		if err := a.redis.Do(radix.Cmd(nil, "HINCRBY", balancesKey, data.ItemUser, "-1")); err != nil {
			mlog.From(a.cmp).Error("error decrementing user's balance", ctx, merr.Context(err))
		}
	case "message":
		if a.ghost {
			return
		}
		data, ok := e.Data.(*slack.MessageEvent)
		if !ok || data.User == a.botUserID {
			return
		} else if err := a.processSlackMsg(ctx, data.Channel, data.User, data.Text); err != nil {
			ctx = mctx.Annotate(ctx, "text", data.Text)
			mlog.From(a.cmp).Warn("error processing message", ctx, merr.Context(err))
		}
	}
}

func (a *app) processStellarPayment(payment operations.Payment) {
	ctx := mctx.Annotate(a.cmp.Context(),
		"paymentOpID", payment.ID,
		"paymentCursor", payment.PT,
		"paymentFrom", payment.From,
		"paymentCode", payment.Code,
		"paymentIssuer", payment.Issuer,
		"paymentAmount", payment.Amount,
	)
	mlog.From(a.cmp).Info("processing incoming stellar transaction", ctx)

	if payment.Code != "CRYPTICBUCK" || payment.Issuer != a.stellar.kp.Address() {
		mlog.From(a.cmp).Warn("payment is not in buckaroo's currency", ctx)
		return
	}

	tx, err := a.stellar.client.TransactionDetail(payment.GetTransactionHash())
	if err != nil {
		mlog.From(a.cmp).Warn("failed to retrieve operation's tx", ctx, merr.Context(err))
		return
	}

	ctx = mctx.Annotate(ctx, "memo", tx.Memo)
	suffix := "*" + a.stellar.domain
	if !strings.HasSuffix(tx.Memo, suffix) {
		// if they don't fill the memo correctly, don't distribute the money.
		mlog.From(a.cmp).Warn("incoming stellar transaction has invalid memo", ctx)
		return
	}

	userName := strings.TrimSuffix(tx.Memo, suffix)
	user, err := a.getUserByName(userName)
	if err != nil {
		mlog.From(a.cmp).Warn("error retrieving user info", ctx, merr.Context(err))
		return
	} else if user == nil { // not sure if this happens, but whatevs
		mlog.From(a.cmp).Warn("incoming stellar transaction destined for invalid user", ctx)
		return
	}

	amount, err := strconv.ParseFloat(payment.Amount, 64)
	if err != nil {
		mlog.From(a.cmp).Warn("error parsing tx amount", ctx, merr.Context(err))
		return
	} else if float64(int64(amount)) != amount {
		mlog.From(a.cmp).Warn("amount is not a whole number", ctx)
		return
	}

	// TODO is it possible to reject a stellar tx? If so we should do that for
	// any of the above cases

	ctx = mctx.Annotate(ctx, "dstUserID", user.ID, "dstUserName", user.Name)
	mlog.From(a.cmp).Info("incrementing user's account", ctx)
	err = a.redis.Do(radix.FlatCmd(nil, "HINCRBY", balancesKey, user.ID, int64(amount)))
	if err != nil {
		mlog.From(a.cmp).Warn("failed to increment user's balance", ctx, merr.Context(err))
		return
	}
}

const lastCursorKey = "lastCursor"

func (a *app) spin(ctx context.Context, lastCursor string) {
	if err := a.refreshUsersByName(); err != nil {
		mlog.From(a.cmp).Fatal("failed to retrieve full user list", a.cmp.Context(), ctx, merr.Context(err))
	}

	paymentCh := a.stellar.receivePayments(ctx, lastCursor)
	for {
		select {
		case e := <-a.slackClient.RTM.IncomingEvents:
			a.processSlackEvent(e)
		case payment := <-paymentCh:
			a.processStellarPayment(payment)
			pt := payment.PagingToken()
			if err := a.redis.Do(radix.Cmd(nil, "SET", lastCursorKey, pt)); err != nil {
				mlog.From(a.cmp).Error("could not set lastCursorKey",
					mctx.Annotate(ctx, "lastCursor", pt),
					merr.Context(err))
			}
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	cmp := m.RootServiceComponent()
	a := app{
		cmp:         cmp,
		channels:    map[string]*slack.Channel{},
		users:       map[string]*slack.User{},
		usersByName: map[string]*slack.User{},
		redis:       instRedis(cmp),
		stellar:     instStellarServer(cmp),
		slackClient: instSlackClient(cmp),
	}

	ghost := mcfg.Bool(cmp, "ghost",
		mcfg.ParamUsage("if set then buckaroo will ignore all messages directed at him"))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		a.ghost = *ghost
		if a.ghost {
			mlog.From(cmp).Info("ghost mode is enabled, wooOOOoOOOOoooOOOOOOoooo", ctx)
		}
		return nil
	})

	mrun.InitHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("getting bot user info", ctx)
		res, err := a.slackClient.Client.AuthTest()
		if err != nil {
			return err
		}
		a.botUser = res.User
		a.botUserID = res.UserID
		cmp.Annotate("botUser", a.botUser, "botUserID", a.botUserID)
		mlog.From(cmp).Info("got bot user info", ctx)
		return nil
	})

	spinCtx, cancel := context.WithCancel(context.Background())
	spinWait := make(chan struct{})
	mrun.InitHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("fetching last cursor from redis", ctx)
		var lastCursor string
		mn := radix.MaybeNil{Rcv: &lastCursor}
		if err := a.redis.Do(radix.Cmd(&mn, "GET", lastCursorKey)); err != nil {
			return merr.Wrap(err, ctx)
		}

		mlog.From(cmp).Info("beginning app loop",
			mctx.Annotate(ctx, "lastCursor", lastCursor))
		go func() {
			a.spin(spinCtx, lastCursor)
			close(spinWait)
		}()
		return nil
	})
	mrun.ShutdownHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("shutting down main thread", ctx)
		cancel()
		select {
		case <-spinWait:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	m.Exec(cmp)
}
