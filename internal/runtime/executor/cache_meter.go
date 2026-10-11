package executor

import (
	"context"

	"github.com/nguyenha935/kiro-cpa-plugin/internal/cachemeter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type cacheMeterKey struct{}

type cacheMeterInput struct {
	scope   string
	model   string
	request []byte
}

// withCacheMeter carries what the cache meter needs into the stream goroutine
// without widening streamToChannel's signature. request is the body after
// translation (Claude or OpenAI chat shape), scope the Kiro credential.
func withCacheMeter(ctx context.Context, scope, kiroModel string, request []byte) context.Context {
	return context.WithValue(ctx, cacheMeterKey{}, cacheMeterInput{scope: scope, model: kiroModel, request: request})
}

// applyCacheMeter meters a completed turn when the context carries a request.
func applyCacheMeter(ctx context.Context, detail usage.Detail) usage.Detail {
	in, ok := ctx.Value(cacheMeterKey{}).(cacheMeterInput)
	if !ok {
		return detail
	}
	return cachemeter.Apply(in.scope, in.model, in.request, detail)
}
