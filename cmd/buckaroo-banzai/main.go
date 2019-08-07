package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mediocregopher/mediocre-go-lib/m"
	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/nlopes/slack"
	"github.com/stellar/go/protocols/horizon/operations"

	"buckaroo-banzai/bank"
	"buckaroo-banzai/stellar"
)

const exportProtocolStellar = "stellar"

var gitRef string

type app struct {
	cmp *mcmp.Component

	bank        bank.ExportingBank
	slackClient *slackClient
	stellar     *stellarServer

	// if true then buckaroo won't speak or listen to anyone speaking to him.
	ghost bool
}

func (a *app) helpMsg(isIM bool) string {
	var suffix string
	if !isIM {
		suffix = "s"
	}

	strb := new(strings.Builder)
	fmt.Fprintf(strb, "sup nerd%s! I'm Buckaroo Bonzai, a very cool guy and the sole owner of the CRYPTICBUCK cryptocurrency bank, housed right here in the cryptic slack group.\n", suffix)
	fmt.Fprintf(strb, "-----\n*CRYPTICBUCKs*\n")
	fmt.Fprintf(strb, "your slack account earns one CRYPTICBUCK whenever someone adds an emoji reaction to one of your messages. by @'ing or DMing me you can give them to other people in the slack team, or withdraw them into a stellar wallet.\n")

	fmt.Fprintf(strb, "-----\n*Commands*\n```")
	fmt.Fprintf(strb, `
// I will respond with your bank balance
@%s balance

// transfer your CRYPTICBUCKs to another user's slack bank
@%s give <amount> @<user>

// withdraw CRYPTICBUCKs to <stellar/federated address>
@%s withdraw <amount> <stellar/federated address> [<memo>]
`, a.slackClient.botUser, a.slackClient.botUser, a.slackClient.botUser)
	fmt.Fprintf(strb, "```\n")

	fmt.Fprintf(strb, "-----\n*Withdrawing*\n")
	fmt.Fprintf(strb, "to withdraw CRYPTICBUCKs into your own stellar wallet (e.g. keybase) you must first add a trustline with the issuer `%s` and the asset `CRYPTICBUCK` to your wallet. once done, use the `withdraw` command to send yourself those sweet sweet cryptos.\n", a.stellar.kp.Address())

	fmt.Fprintf(strb, "-----\n*Depositing*\n")
	fmt.Fprintf(strb, "to deposit CRYPTICBUCKs from your stellar wallet back into a slack account simply send the tokens to the stellar address `<username>*bucks.cryptic.io`. The username _must_ be the same as the slack username (the one used when you @ someone).")

	return strb.String()
}

// wow, regexes are fucking ugly
var slackUnFormatRegex = regexp.MustCompile(`([^*]+)\*<[^|]+\|([^>]+)>`)

func (a *app) processSlackMsg(ctx context.Context, channelID, userID, msg string) error {
	if userID == a.slackClient.botUserID {
		// ignore messages sent by the bot itself. Can happen during testing
		// when there's two running bots
		return nil
	}

	ctx = mctx.Annotate(ctx, "channelID", channelID)
	channel, err := a.slackClient.getChannel(channelID)
	if err != nil {
		return merr.Wrap(err, ctx)
	}
	isIM := channel.IsIM
	ctx = mctx.Annotate(ctx, "userID", userID, "channel", channel.Name, "isIM", isIM)

	user, err := a.slackClient.getUser(userID)
	if err != nil {
		return merr.Wrap(err, ctx)
	}
	ctx = mctx.Annotate(ctx, "user", user.Name)

	msg = strings.TrimSpace(msg)
	prefix := "<@" + a.slackClient.botUserID + ">"
	if !strings.HasPrefix(msg, prefix) && !isIM {
		return nil
	}
	msg = strings.TrimPrefix(msg, prefix)
	fields := strings.Fields(msg)

	sendMsg := func(channelID string, prefixName bool, str string, args ...interface{}) {
		str = fmt.Sprintf(str, args...)
		if !channel.IsIM && prefixName {
			str = fmt.Sprintf("<@%s> %s", userID, str)
		}
		outMsg := a.slackClient.RTM.NewOutgoingMessage(str, channelID)
		a.slackClient.RTM.SendMessage(outMsg)
	}

	if len(fields) < 1 {
		sendMsg(channelID, false, a.helpMsg(isIM))
		return nil
	}

	var outErr error
	switch strings.ToLower(fields[0]) {
	case "ref":
		sendMsg(channelID, true, "Current git ref is `%s`", gitRef)

	case "balance":
		ctx = mctx.Annotate(ctx, "command", "balance")
		mlog.From(a.cmp).Info("getting user balance", ctx)
		balance, err := a.bank.Balance(userID)
		if err != nil {
			outErr = err
			break
		}
		if balance == 0 {
			sendMsg(channelID, true, "sorry champ, you don't have any CRYPTICBUCKs :( if you're having trouble getting CRYPTICBUCKs, try being cool!")
		} else if balance == 1 {
			sendMsg(channelID, true, "you have 1 CRYPTICBUCK!")
		} else if balance < 0 {
			sendMsg(channelID, true, "you have %d CRYPTICBUCKs! that's not even possible :face_with_monocle:", balance)
		} else {
			sendMsg(channelID, true, "you have %d CRYPTICBUCKs!", balance)
		}

	case "give":
		if len(fields) != 3 {
			sendMsg(channelID, false, a.helpMsg(isIM))
			break
		}
		ctx = mctx.Annotate(ctx, "amount", fields[1])
		amount, err := strconv.Atoi(fields[1])
		if err != nil {
			outErr = err
			break
		}

		ctx = mctx.Annotate(ctx, "command", "give", "dstUserID", fields[2])
		dstUser, err := a.slackClient.getUser(fields[2])
		if err != nil {
			outErr = err
			break
		}
		ctx = mctx.Annotate(ctx, "dstUser", dstUser.Name, "dstUserID", dstUser.ID)

		if dstUser.ID == userID {
			sendMsg(channelID, true, "quit playing with yourself, kid")
			break
		}

		mlog.From(a.cmp).Info("giving bucks", ctx)
		dstBalance, _, err := a.bank.Transfer(dstUser.ID, user.ID, amount)
		if err != nil {
			outErr = err
			break
		}

		sendMsg(channelID, true, "you gave <@%s> %d CRYPTICBUCK(s) :money_with_wings:", dstUser.ID, amount)

		// don't dm a bot, it errors out
		if dstUser.IsBot {
			break
		}

		imChannelID, err := a.slackClient.getIMChannel(dstUser.ID)
		if err != nil {
			outErr = err
			break
		}
		sendMsg(imChannelID, true, "your friend <@%s> gave you %d CRYPTICBUCKs, giving you a total of %d", userID, amount, dstBalance)

	case "withdraw":
		if l := len(fields); l < 3 || l > 4 {
			sendMsg(channelID, false, a.helpMsg(isIM))
			break
		}

		amount, err := strconv.Atoi(fields[1])
		if err != nil {
			outErr = err
			break
		} else if amount <= 0 {
			outErr = merr.New("amount must be greater than 0")
			break
		}
		amountStr := strconv.Itoa(amount)
		ctx = mctx.Annotate(ctx, "command", "send", "amount", amount)

		addr := fields[2]
		addr = slackUnFormatRegex.ReplaceAllString(addr, `${1}*${2}`)

		var memo string
		if len(fields) == 4 {
			memo = fields[3]
			ctx = mctx.Annotate(ctx, "memo", memo)
		}

		mlog.From(a.cmp).Info("constructing send XDR", ctx)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var txXDR string
		txXDR, outErr = a.stellar.client.MakeSendXDR(ctx, stellar.SendOpts{
			From:        a.stellar.kp,
			To:          addr,
			Memo:        memo,
			AssetCode:   "CRYPTICBUCK",
			AssetIssuer: a.stellar.kp.Address(),
			Amount:      amountStr,
		})
		if outErr != nil {
			break
		}

		mlog.From(a.cmp).Info("submitting XDR to the bank", ctx)
		var txID string
		txID, outErr = a.bank.SubmitExport(bank.Export{
			FromUserID:      userID,
			Amount:          amount,
			Protocol:        exportProtocolStellar,
			ProtocolPayload: txXDR,
		})
		if outErr != nil {
			break
		}

		ctx = mctx.Annotate(ctx, "txID", txID)
		mlog.From(a.cmp).Info("XDR successfully submitted", ctx)

		sendMsg(channelID, true, "you withdrew `%s` %d CRYPTICBUCK(s) :money_with_wings: :money_with_wings: You'll get a DM when the transaction has been successfully submitted to the network", addr, amount)

	default:
		sendMsg(channelID, false, a.helpMsg(isIM))
	}

	if outErr != nil {
		outErr = merr.Wrap(outErr, ctx)
		sendMsg(channelID, true, "what a bummer: %s", outErr)
		return outErr
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
		if _, err := a.bank.Incr(data.ItemUser, 1); err != nil {
			mlog.From(a.cmp).Error("error incrementing user's balance", ctx, merr.Context(err))
		}
	case "reaction_removed":
		data, ok := e.Data.(*slack.ReactionRemovedEvent)
		if !ok || data.User == data.ItemUser {
			return
		}
		ctx = mctx.Annotate(ctx, "user", data.ItemUser)
		mlog.From(a.cmp).Info("decrementing user's balance", ctx)

		// it's possible for the user to not have enough funds to decrement, for
		// example if they received a reaction, gave the earned buck to someone
		// else, then the reaction was removed. I guess this is fine?
		if _, err := a.bank.Incr(data.ItemUser, -1); err != nil && !bank.IsNotEnoughFunds(err) {
			mlog.From(a.cmp).Error("error decrementing user's balance", ctx, merr.Context(err))
		}
	case "message":
		if a.ghost {
			return
		}
		data, ok := e.Data.(*slack.MessageEvent)
		if !ok || data.User == a.slackClient.botUserID {
			return
		} else if err := a.processSlackMsg(ctx, data.Channel, data.User, data.Text); err != nil {
			ctx = mctx.Annotate(ctx, "text", data.Text)
			mlog.From(a.cmp).Warn("error processing message", ctx, merr.Context(err))
		}
	}
}

func (a *app) processSlackEvents(ctx context.Context) {

	for {
		select {
		case e := <-a.slackClient.RTM.IncomingEvents:
			a.processSlackEvent(e)
		case <-ctx.Done():
			return
		}
	}
}

///////////////////////////////////////////////////////////////////////////////

func (a *app) processStellarPayment(ctx context.Context, payment operations.Payment) error {
	mlog.From(a.cmp).Info("processing incoming stellar transaction", ctx)

	if payment.Code != "CRYPTICBUCK" || payment.Issuer != a.stellar.kp.Address() {
		return merr.New("payment is not in buckaroo's currency", a.cmp.Context(), ctx)
	}

	tx, err := a.stellar.client.TransactionDetail(payment.GetTransactionHash())
	if err != nil {
		return merr.New("failed to retrieve operation's tx", a.cmp.Context(), ctx)
	}

	ctx = mctx.Annotate(ctx, "memo", tx.Memo)
	userName := strings.TrimSuffix(tx.Memo, "*"+a.stellar.domain)
	user, err := a.slackClient.getUserByName(userName)
	if err != nil {
		return merr.Wrap(err, a.cmp.Context(), ctx)
	} else if user == nil { // not sure if this happens, but whatevs
		return merr.New("incoming stellar transaction destined for invalid user", a.cmp.Context(), ctx)
	}

	amount, err := strconv.ParseFloat(payment.Amount, 64)
	if err != nil {
		return merr.Wrap(err, a.cmp.Context(), ctx)
	} else if float64(int(amount)) != amount {
		return merr.New("amount is not a whole number", a.cmp.Context(), ctx)
	}

	// TODO is it possible to reject a stellar tx? If so we should do that for
	// any of the above cases

	ctx = mctx.Annotate(ctx, "dstUserID", user.ID, "dstUserName", user.Name, "amount", amount)
	mlog.From(a.cmp).Info("incrementing user's account", ctx)
	if _, err := a.bank.Incr(user.ID, int(amount)); err != nil {
		return merr.Wrap(err, a.cmp.Context(), ctx)
	}

	imChannel, err := a.slackClient.getIMChannel(user.ID)
	if err != nil {
		return merr.Wrap(err, a.cmp.Context(), ctx)
	}

	msgStr := fmt.Sprintf("%d CRYPTICBUCK(s) were deposited to your account :moneybag:\n", int(amount))
	if tx.Memo != "" {
		msgStr += fmt.Sprintf("memo: %q\n", tx.Memo)
	}
	msgStr += fmt.Sprintf("sending address: `%s`", tx.Account)
	outMsg := a.slackClient.RTM.NewOutgoingMessage(msgStr, imChannel)
	a.slackClient.RTM.SendMessage(outMsg)

	return nil
}

///////////////////////////////////////////////////////////////////////////////

func (a *app) processExport(ctx context.Context, e bank.ExportInProgress) error {
	ctx = e.Annotate(ctx)
	if e.Protocol != exportProtocolStellar {
		return merr.New("unknown export protocol", a.cmp.Context(), ctx)
	}

	mlog.From(a.cmp).Info("submitting stellar tx", ctx)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := a.stellar.client.SubmitTransactionXDR(ctx, e.ProtocolPayload)
	if err != nil {
		return merr.Wrap(err, a.cmp.Context(), ctx)
	}

	txLink := res.Links.Transaction.Href
	ctx = mctx.Annotate(ctx, "stellarTXLink", txLink)
	mlog.From(a.cmp).Info("stellar tx successfully submitted", ctx)

	if err := e.Ack(); err != nil {
		// if there is an error acking, don't message the user, it'll just cause
		// them to potentially get a duplicate message when the export is
		// retried later.
		return merr.Wrap(err, a.cmp.Context(), ctx)
	}

	imChannel, err := a.slackClient.getIMChannel(e.FromUserID)
	if err != nil {
		mlog.From(a.cmp).Warn("could not retrieve user IM channel to send tx success msg", ctx, merr.Context(err))
		// this isn't a big deal, the tx was successful, just bail
		return nil
	}

	msgStr := fmt.Sprintf("your transaction of %d CRYPTICBUCK(s) was successful!\n%s", e.Amount, txLink)
	outMsg := a.slackClient.RTM.NewOutgoingMessage(msgStr, imChannel)
	a.slackClient.RTM.SendMessage(outMsg)

	return nil
}

func (a *app) processExports(ctx context.Context, ch chan bank.ExportInProgress) {
	for {
		select {
		case exportInProg := <-ch:
			if err := a.processExport(ctx, exportInProg); err != nil {
				mlog.From(a.cmp).Error("error encountered processing export", ctx, merr.Context(err))
			}
		case <-ctx.Done():
			return
		}
	}
}

///////////////////////////////////////////////////////////////////////////////

func main() {
	cmp := m.RootServiceComponent()
	a := app{
		cmp:         cmp,
		bank:        bank.Inst(cmp),
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

	runCtx, cancel := context.WithCancel(context.Background())
	wg := new(sync.WaitGroup)
	mrun.InitHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("refreshing list of slack users")
		if err := a.slackClient.refreshUsersByName(true); err != nil {
			mlog.From(a.cmp).Fatal("failed to retrieve full user list", a.cmp.Context(), ctx, merr.Context(err))
		}

		mlog.From(cmp).Info("starting main threads")
		wg.Add(1)
		go func() {
			defer wg.Done()
			mlog.From(cmp).Info("starting thread to process slack events", ctx)
			a.processSlackEvents(runCtx)
			mlog.From(cmp).Info("stopping thread to process slack events", ctx)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			mlog.From(cmp).Info("starting thread to process incoming stellar payments", ctx)
			a.stellar.receivePayments(runCtx, a.processStellarPayment)
			mlog.From(cmp).Info("stopping thread to process incoming stellar payments", ctx)
		}()

		exportCh := make(chan bank.ExportInProgress)
		wg.Add(1)
		go func() {
			defer wg.Done()
			mlog.From(cmp).Info("starting thread to read submitted exports from the bank", ctx)
			a.processExports(runCtx, exportCh)
			mlog.From(cmp).Info("stopping thread to read submitted exports from the bank", ctx)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			mlog.From(cmp).Info("starting thread to consume submitted exports", ctx)
			for {
				err := a.bank.ConsumeExports(runCtx, exportCh)
				if merr.Base(err) == context.Canceled {
					break
				} else if err != nil {
					mlog.From(cmp).Error("error consuming exports", ctx, merr.Context(err))
					time.Sleep(1 * time.Second)
				}
			}
			mlog.From(cmp).Info("stopping thread to consume submitted exports", ctx)
		}()

		return nil
	})

	mrun.ShutdownHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("shutting down main threads", ctx)
		cancel()
		wg.Wait()
		return nil
	})

	m.Exec(cmp)
}
