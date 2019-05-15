package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mediocregopher/mediocre-go-lib/m"
	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/mediocregopher/radix/v3"
	"github.com/nlopes/slack"
	"github.com/stellar/go/keypair"
)

type slackClient struct {
	Client *slack.Client
	RTM    *slack.RTM
}

func withSlackClient(parent context.Context) (context.Context, *slackClient) {
	ctx := mctx.NewChild(parent, "slack")
	client := new(slackClient)

	ctx, token := mcfg.WithRequiredString(ctx, "token", "API token for the buckaroo bonzai bot")
	ctx = mrun.WithStartHook(ctx, func(context.Context) error {
		client.Client = slack.New(*token)
		client.RTM = client.Client.NewRTM()
		go client.RTM.ManageConnection()
		return nil
	})

	return mctx.WithChild(parent, ctx), client
}

func withRedis(parent context.Context) (context.Context, radix.Client) {
	ctx := mctx.NewChild(parent, "redis")
	client := &struct {
		radix.Client
	}{}

	ctx, addr := mcfg.WithString(ctx, "addr", "127.0.0.1:6379", "Address redis is listening on")
	ctx, poolSize := mcfg.WithInt(ctx, "pool-size", 10, "Number of connections in pool")
	ctx = mrun.WithStartHook(ctx, func(context.Context) error {
		var err error
		client.Client, err = radix.NewPool("tcp", *addr, *poolSize)
		return err
	})
	return mctx.WithChild(parent, ctx), client
}

type app struct {
	ctx                context.Context
	botUserID, botUser string

	channels map[string]*slack.Channel
	users    map[string]*slack.User

	slackClient *slackClient
	redis       radix.Client
	stellar     *stellar
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

const errNotEnoughBucks = "you aint got that kind of scratch"

// Keys:[balancesKey] Args:[dstUser, srcUser, amount]
var giveCmd = radix.NewEvalScript(1, `
	local amount = tonumber(ARGV[3])
	local srcAmount = tonumber(redis.call("HGET", KEYS[1], ARGV[2]))
	if (srcAmount < amount) then
		return redis.error_reply("`+errNotEnoughBucks+`")
	end

	redis.call("HINCRBY", KEYS[1], ARGV[1], amount)
	redis.call("HINCRBY", KEYS[1], ARGV[2], -1*amount)
	return "OK"
`)

func (a *app) processMsg(channelID, userID, msg string) error {
	ctx := mctx.Annotate(a.ctx, "channelID", channelID)
	channel, err := a.getChannel(channelID)
	if err != nil {
		return merr.Wrap(err, ctx)
	}
	ctx = mctx.Annotate(ctx, "userID", userID, "channel", channel.Name)

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
	case "balance":
		ctx = mctx.Annotate(ctx, "command", "balance")
		mlog.Info("getting user balance", ctx)
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
		ctx = mctx.Annotate(ctx, "command", "give")
		srcUser, err := a.getUser(userID)
		if err != nil {
			outErr = err
			break
		}

		ctx = mctx.Annotate(ctx, "user", srcUser.Name, "dstUserID", fields[1])
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

		mlog.Info("giving bucks", ctx)
		if err = a.redis.Do(giveCmd.Cmd(
			nil, balancesKey, dstUser.ID, srcUser.ID, strconv.Itoa(amount),
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

func (a *app) spin() {
	for {
		e := <-a.slackClient.RTM.IncomingEvents

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
				continue
			}
			ctx := mctx.Annotate(a.ctx, "user", data.ItemUser)
			mlog.Info("incrementing user's balance", ctx)
			if err := a.redis.Do(radix.Cmd(nil, "HINCRBY", balancesKey, data.ItemUser, "1")); err != nil {
				mlog.Error("error incrementing user's balance", ctx, merr.Context(err))
			}
		case "reaction_removed":
			data, ok := e.Data.(*slack.ReactionRemovedEvent)
			if !ok || data.User == data.ItemUser {
				continue
			}
			ctx := mctx.Annotate(a.ctx, "user", data.ItemUser)
			mlog.Info("decrementing user's balance", ctx)
			if err := a.redis.Do(radix.Cmd(nil, "HINCRBY", balancesKey, data.ItemUser, "-1")); err != nil {
				mlog.Error("error decrementing user's balance", ctx, merr.Context(err))
			}
		case "message":
			data, ok := e.Data.(*slack.MessageEvent)
			if !ok {
				continue
			}
			if err := a.processMsg(data.Channel, data.User, data.Text); err != nil {
				ctx := mctx.Annotate(a.ctx, "text", data.Text)
				mlog.Warn("error processing message", ctx, merr.Context(err))
			}
		}
	}
}

func main() {
	ctx := m.ServiceContext()

	// for convenience, add a keygen option which will generate a new key, print
	// it, then exit
	var keygen *bool
	ctx, keygen = mcfg.WithBool(ctx, "key-gen", "If set, generate a new stellar seed/address, print them, then exit")
	ctx = mrun.WithStartHook(ctx, func(innerCtx context.Context) error {
		if !*keygen {
			return nil
		}
		pair, err := keypair.Random()
		if err != nil {
			return merr.Wrap(err, ctx, innerCtx)
		}

		mlog.Info("keypair generated", mctx.Annotate(ctx,
			"address", pair.Address(),
			"seed", pair.Seed(),
		))
		return merr.New("exiting due to key-gen param", ctx, innerCtx)
	})

	var a app
	a.channels = map[string]*slack.Channel{}
	a.users = map[string]*slack.User{}
	ctx, a.slackClient = withSlackClient(ctx)
	ctx, a.redis = withRedis(ctx)
	ctx, a.stellar = withStellar(ctx)
	a.ctx = ctx
	ctx = mrun.WithStartHook(ctx, func(context.Context) error {
		go a.spin()
		return nil
	})

	ctx = mrun.WithStartHook(ctx, func(innerCtx context.Context) error {
		mlog.Info("getting bot user info", ctx, innerCtx)
		res, err := a.slackClient.Client.AuthTest()
		if err != nil {
			return err
		}
		a.botUser = res.User
		a.botUserID = res.UserID
		return nil
	})

	m.StartWaitStop(ctx)
}
