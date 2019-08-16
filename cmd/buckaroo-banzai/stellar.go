package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/mdb/mredis"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mhttp"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/mediocregopher/radix/v3"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/protocols/horizon/operations"

	"buckaroo-banzai/stellar"
)

const federationPath = "/api/federation"

type stellarServer struct {
	cmp       *mcmp.Component
	kp        *keypair.Full
	tokenName string
	domain    string
	client    *stellar.Client

	// stellar needs its own redis instance in order to store the seen
	// lastCursor
	redis *mredis.Redis

	*http.ServeMux
}

func instStellarServer(parent *mcmp.Component) *stellarServer {
	cmp := parent.Child("stellar")
	s := &stellarServer{
		cmp:      cmp,
		ServeMux: http.NewServeMux(),
		client:   stellar.InstClient(cmp, false),
		kp:       stellar.InstKeyPair(cmp),
		redis:    mredis.InstRedis(cmp),
	}

	s.ServeMux.HandleFunc("/.well-known/stellar.toml", s.tomlHandler)
	s.ServeMux.HandleFunc(federationPath, s.federationHandler)

	tokenName := mcfg.String(cmp, "token-name",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Name of the token to be issued"))
	domain := mcfg.String(s.cmp, "domain",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Domain the server will be served from"))

	mrun.InitHook(s.cmp, func(ctx context.Context) error {
		s.tokenName = *tokenName
		s.domain = *domain
		s.cmp.Annotate("tokenName", s.tokenName, "domain", s.domain)
		return nil
	})

	mhttp.InstListeningServer(s.cmp, s)
	return s
}

var stellarTOMLTPL = template.Must(template.New("").Parse(`
ACCOUNTS=["{{.Address}}"]
FEDERATION_SERVER="https://{{.FederationAddr}}"

[[CURRENCIES]]
CODE="{{.TokenName}}"
ISSUER="{{.Address}}"
DISPLAY_DECIMALS=0
IS_UNLIMITED=true
NAME="{{.TokenName}}"
DESC="{{.TokenName}}s are given to members of the Cryptic group by our resident Token Lord, Buckaroo Bonzai. <script>alert('fix your shit lol');</script>"
CONDITIONS="{{.TokenName}}s are priceless and anybody trading them is a fool."
`))

func (s *stellarServer) tomlHandler(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	rw.Header().Set("Content-Type", "text/toml")

	err := stellarTOMLTPL.Execute(rw, struct{ TokenName, Address, FederationAddr string }{
		TokenName:      s.tokenName,
		Address:        s.kp.Address(),
		FederationAddr: s.domain + federationPath,
	})
	if err != nil {
		mlog.From(s.cmp).Error("error executing toml template",
			r.Context(), merr.Context(err))
	}
}

const notFoundStr = `{"detail":"not found"}`

func (s *stellarServer) federationHandler(rw http.ResponseWriter, r *http.Request) {
	if r.FormValue("type") != "name" {
		http.Error(rw, notFoundStr, 404)
		return
	}

	q := r.FormValue("q")
	if !strings.HasSuffix(q, "*"+s.domain) {
		http.Error(rw, notFoundStr, 404)
		return
	}
	userName := strings.TrimSuffix(q, "*"+s.domain)

	// We don't want to actually check if the username is a member of the slack
	// channel and return based on that, because someone would be able to use
	// that to enumerate all the users of a group. So just always return the
	// address, if someone sends money to the wrong account then.... thanks!
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]string{
		"stellar_address": q,
		"account_id":      s.kp.Address(),
		"memo_type":       "text",
		"memo":            userName,
	})
}

const lastCursorKey = "buckaroo-banzai:stellar:lastCursor"

func (s *stellarServer) receivePayments(ctx context.Context, fn func(context.Context, operations.Payment) error) {
	// TODO this should use a redis stream like withdrawing payments does, so we
	// can be sure to properly consume all payments

	var lastCursor string
	mlog.From(s.cmp).Info("fetching last cursor from redis", ctx)
	for {
		mn := radix.MaybeNil{Rcv: &lastCursor}
		if err := s.redis.Do(radix.Cmd(&mn, "GET", lastCursorKey)); err != nil {
			mlog.From(s.cmp).Warn("error fetching last cursor", s.cmp.Context(), ctx, merr.Context(err))
			time.Sleep(1 * time.Second)
		} else {
			break
		}
	}
	mlog.From(s.cmp).Info("fetched last cursor from redis",
		mctx.Annotate(ctx, "lastCursor", lastCursor))

	for {
		req := horizonclient.OperationRequest{
			ForAccount: s.kp.Address(),
			Cursor:     lastCursor,
		}

		err := s.client.StreamPayments(ctx, req, func(op operations.Operation) {
			ctx := mctx.Annotate(ctx,
				"lastCursor", lastCursor,
				"opCursor", op.PagingToken())

			var opT operations.Payment
			var ok bool
			if opT, ok = op.(operations.Payment); ok && opT.To == s.kp.Address() && opT.From != s.kp.Address() {
				ctx = mctx.Annotate(ctx,
					"paymentOpID", opT.ID,
					"paymentCursor", opT.PT,
					"paymentFrom", opT.From,
					"paymentCode", opT.Code,
					"paymentIssuer", opT.Issuer,
					"paymentAmount", opT.Amount,
					"paymentTXHash", opT.GetTransactionHash(),
				)
				if err := fn(ctx, opT); err != nil {
					mlog.From(s.cmp).Warn("error processing Payment", ctx, merr.Context(err))
				}
			} else if !ok {
				mlog.From(s.cmp).Warn("unsupported operation type",
					mctx.Annotate(ctx, "op", fmt.Sprintf("%#v", op)))
			}

			lastCursor = op.PagingToken()
			if err := s.redis.Do(radix.Cmd(nil, "SET", lastCursorKey, lastCursor)); err != nil {
				mlog.From(s.cmp).Error("could not set lastCursorKey to op's cursor", merr.Context(err))
			}
		})
		if err == context.Canceled || ctx.Err() != nil {
			return
		} else if err == nil {
			// sometimes this happens, I don't know why?
			mlog.From(s.cmp).Warn("nil error from StreamPayments :shrug:", ctx)
			continue
		}

		mlog.From(s.cmp).Warn("error while streaming transactions", ctx, merr.Context(err))
	}
}
