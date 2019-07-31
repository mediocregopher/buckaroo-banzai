package main

import (
	"context"
	"strings"
	"sync"

	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/nlopes/slack"
)

type slackClient struct {
	cmp *mcmp.Component

	Client *slack.Client
	RTM    *slack.RTM

	botUserID, botUser string

	l           sync.Mutex
	channels    map[string]*slack.Channel
	users       map[string]*slack.User
	usersByName map[string]*slack.User
	ims         map[string]string
}

func instSlackClient(parent *mcmp.Component) *slackClient {
	cmp := parent.Child("slack")
	client := &slackClient{
		cmp:         cmp,
		channels:    map[string]*slack.Channel{},
		users:       map[string]*slack.User{},
		usersByName: map[string]*slack.User{},
		ims:         map[string]string{},
	}

	token := mcfg.String(cmp, "token",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("API token for the buckaroo bonzai bot"))
	mrun.InitHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("connecting to slack", ctx)
		client.Client = slack.New(*token)
		client.RTM = client.Client.NewRTM()
		go client.RTM.ManageConnection()

		mlog.From(cmp).Info("getting bot user info", ctx)
		res, err := client.Client.AuthTest()
		if err != nil {
			return err
		}
		client.botUser = res.User
		client.botUserID = res.UserID
		cmp.Annotate("botUser", client.botUser, "botUserID", client.botUserID)
		mlog.From(cmp).Info("got bot user info", ctx)

		return nil
	})

	mrun.ShutdownHook(cmp, func(ctx context.Context) error {
		mlog.From(cmp).Info("shutting down slack", ctx)
		return client.RTM.Disconnect()
	})

	return client
}

func (sc *slackClient) getChannel(id string) (*slack.Channel, error) {
	sc.l.Lock()
	defer sc.l.Unlock()

	if channel, ok := sc.channels[id]; ok {
		return channel, nil
	}
	channel, err := sc.Client.GetConversationInfo(id, true)
	if err == nil {
		sc.channels[id] = channel
	}
	return channel, merr.Wrap(err, sc.cmp.Context())
}

func (sc *slackClient) getUser(id string) (*slack.User, error) {
	sc.l.Lock()
	defer sc.l.Unlock()

	id = strings.TrimPrefix(id, "<")
	id = strings.TrimPrefix(id, "@")
	id = strings.TrimSuffix(id, ">")
	if user, ok := sc.users[id]; ok {
		return user, nil
	}

	user, err := sc.Client.GetUserInfo(id)
	if err == nil {
		sc.users[id] = user
	}
	return user, merr.Wrap(err, sc.cmp.Context())
}

func (sc *slackClient) refreshUsersByName(lock bool) error {
	if lock {
		sc.l.Lock()
		defer sc.l.Unlock()
	}

	users, err := sc.Client.GetUsers()
	if err != nil {
		return merr.Wrap(err, sc.cmp.Context())
	}

	sc.usersByName = make(map[string]*slack.User, len(users))
	for i, user := range users {
		sc.usersByName[user.Name] = &users[i]
	}
	return nil
}

func (sc *slackClient) getUserByName(name string) (*slack.User, error) {
	sc.l.Lock()
	defer sc.l.Unlock()

	user, ok := sc.usersByName[name]
	if ok {
		return user, nil
	} else if err := sc.refreshUsersByName(false); err != nil {
		return nil, merr.Wrap(err, sc.cmp.Context())
	}
	user, ok = sc.usersByName[name]
	if !ok {
		return nil, merr.New("user not found",
			mctx.Annotate(sc.cmp.Context(), "user", user))
	}
	return user, nil
}

func (sc *slackClient) getIMChannel(userID string) (string, error) {
	sc.l.Lock()
	defer sc.l.Unlock()

	channel, ok := sc.ims[userID]
	if ok {
		return channel, nil
	}

	_, _, channel, err := sc.Client.OpenIMChannel(userID)
	if err != nil {
		return "", merr.Wrap(err, sc.cmp.Context())
	}

	sc.ims[userID] = channel
	return channel, nil
}
