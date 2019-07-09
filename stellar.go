package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mcmp"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mhttp"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/protocols/horizon/operations"
	"github.com/stellar/go/strkey"
)

const federationPath = "/api/federation"

// TODO make CRYPTICBUCK configurable
type stellar struct {
	cmp    *mcmp.Component
	kp     *keypair.Full
	domain string
	client *horizonclient.Client

	*http.ServeMux
}

func instStellar(parent *mcmp.Component) *stellar {
	s := &stellar{
		cmp:      parent.Child("stellar"),
		ServeMux: http.NewServeMux(),
	}

	s.ServeMux.HandleFunc("/.well-known/stellar.toml", s.tomlHandler)
	s.ServeMux.HandleFunc(federationPath, s.federationHandler)

	var (
		seedStr *string
		domain  *string
		live    *bool
	)
	seedStr = mcfg.String(s.cmp, "seed",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Seed for account which will issue CRYPTICBUCKs"))
	domain = mcfg.String(s.cmp, "domain",
		mcfg.ParamRequired(),
		mcfg.ParamUsage("Domain the server will be served from"))
	live = mcfg.Bool(s.cmp, "live",
		mcfg.ParamUsage("Use the live network."))

	mrun.InitHook(s.cmp, func(ctx context.Context) error {
		seedB, err := strkey.Decode(strkey.VersionByteSeed, *seedStr)
		if err != nil {
			return merr.Wrap(err, s.cmp.Context(), ctx)
		} else if len(seedB) != 32 {
			return merr.New("invalid seed string", s.cmp.Context(), ctx)
		}
		var seedB32 [32]byte
		copy(seedB32[:], seedB)
		pair, err := keypair.FromRawSeed(seedB32)
		if err != nil {
			return merr.Wrap(err, s.cmp.Context(), ctx)
		}
		s.kp = pair
		s.cmp.Annotate("address", s.kp.Address())
		mlog.From(s.cmp).Info("loaded stellar seed", ctx)

		s.domain = *domain
		s.cmp.Annotate("domain", s.domain)

		if *live {
			s.client = horizonclient.DefaultPublicNetClient
		} else {
			s.client = horizonclient.DefaultTestNetClient
		}
		return nil
	})

	mhttp.InstListeningServer(s.cmp, s)
	return s
}

var stellarTOMLTPL = template.Must(template.New("").Parse(`
ACCOUNTS=["{{.Address}}"]
FEDERATION_SERVER="{{.FederationAddr}}"

[[CURRENCIES]]
CODE="CRYPTICBUCK"
ISSUER="{{.Address}}"
DISPLAY_DECIMALS=0
IS_UNLIMITED=true
NAME="CRYPTICBUCK"
DESC="CRYPTICBUCKs are given to members of the Cryptic group by our resident Token Lord, Buckaroo Bonzai. <script>alert('fix your shit lol');</script>"
CONDITIONS="CRYPTICBUCKs are priceless and anybody trading them is a fool."
`))

func (s *stellar) tomlHandler(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	rw.Header().Set("Content-Type", "text/toml")

	err := stellarTOMLTPL.Execute(rw, struct{ Address, FederationAddr string }{
		Address:        s.kp.Address(),
		FederationAddr: s.domain + federationPath,
	})
	if err != nil {
		mlog.From(s.cmp).Error("error executing toml template",
			r.Context(), merr.Context(err))
	}
}

const notFoundStr = `{"detail":"not found"}`

func (s *stellar) federationHandler(rw http.ResponseWriter, r *http.Request) {
	if r.FormValue("type") != "name" {
		http.Error(rw, notFoundStr, 404)
		return
	}

	q := r.FormValue("q")
	if !strings.HasSuffix(q, "*"+s.domain) {
		http.Error(rw, notFoundStr, 404)
		return
	}

	// We don't want to actually check if the username is a member of the slack
	// channel and return based on that, because someone would be able to use
	// that to enumerate all the users of a group. So just always return the
	// address, if someone sends money to the wrong account then.... thanks!
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]string{
		"stellar_address": q,
		"account_id":      s.kp.Address(),
	})
}

func (s *stellar) receivePayments(ctx context.Context, lastCursor string) <-chan operations.Payment {
	ch := make(chan operations.Payment)
	go func() {
		defer close(ch)
		for {
			req := horizonclient.OperationRequest{
				ForAccount: s.kp.Address(),
				Cursor:     lastCursor,
			}
			err := s.client.StreamPayments(ctx, req, func(op operations.Operation) {
				switch opT := op.(type) {
				case operations.Payment:
					ch <- opT
				case operations.PathPayment:
					ch <- opT.Payment
				default:
					mlog.From(s.cmp).Warn("unknown operation type",
						mctx.Annotate(ctx, "op", fmt.Sprintf("%#v", op)))
				}
				lastCursor = op.PagingToken()
			})
			if err == context.Canceled {
				return
			}
			err = merr.Wrap(err)
			mlog.From(s.cmp).Warn("error while streaming transactions",
				ctx, merr.Context(err))
		}
	}()
	return ch
}
