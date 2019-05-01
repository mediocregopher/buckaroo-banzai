package main

import (
	"context"
	"net/http"
	"text/template"

	"github.com/mediocregopher/mediocre-go-lib/mcfg"
	"github.com/mediocregopher/mediocre-go-lib/mctx"
	"github.com/mediocregopher/mediocre-go-lib/merr"
	"github.com/mediocregopher/mediocre-go-lib/mhttp"
	"github.com/mediocregopher/mediocre-go-lib/mlog"
	"github.com/mediocregopher/mediocre-go-lib/mrun"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/strkey"
)

type stellar struct {
	ctx context.Context
	kp  *keypair.Full

	*http.ServeMux
}

func withStellar(parent context.Context) (context.Context, *stellar) {
	s := &stellar{
		ctx:      mctx.NewChild(parent, "stellar"),
		ServeMux: http.NewServeMux(),
	}

	s.ServeMux.Handle("/.well-known/stellar.toml", http.HandlerFunc(s.tomlHandler))

	var seedStr *string
	s.ctx, seedStr = mcfg.WithRequiredString(s.ctx, "seed", "Seed for account which will issue CRYPTICBUCKs")
	s.ctx = mrun.WithStartHook(s.ctx, func(context.Context) error {
		seedB, err := strkey.Decode(strkey.VersionByteSeed, *seedStr)
		if err != nil {
			return merr.Wrap(err, s.ctx)
		} else if len(seedB) != 32 {
			return merr.New("invalid seed string", s.ctx)
		}
		var seedB32 [32]byte
		copy(seedB32[:], seedB)
		pair, err := keypair.FromRawSeed(seedB32)
		if err != nil {
			return merr.Wrap(err, s.ctx)
		}
		s.kp = pair
		s.ctx = mctx.Annotate(s.ctx, "address", s.kp.Address())
		mlog.Info("loaded stellar seed", s.ctx)
		return nil
	})

	s.ctx, _ = mhttp.WithListeningServer(s.ctx, s)

	return mctx.WithChild(parent, s.ctx), s
}

var stellarTOMLTPL = template.Must(template.New("").Parse(`
ACCOUNTS=["{{.Address}}"]

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

	err := stellarTOMLTPL.Execute(rw, struct{ Address string }{
		Address: s.kp.Address(),
	})
	if err != nil {
		mlog.Error("error executing toml template", s.ctx, merr.Context(err))
	}
}
