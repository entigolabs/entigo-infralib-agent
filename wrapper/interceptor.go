package wrapper

import (
	"context"
	"log/slog"

	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type AuthInterceptor struct {
	tokenSource oauth2.TokenSource
	headers     map[string]string
}

func NewAuthInterceptor(tokenSource oauth2.TokenSource, headers map[string]string) *AuthInterceptor {
	return &AuthInterceptor{
		tokenSource: tokenSource,
		headers:     headers,
	}
}

func (i *AuthInterceptor) StreamClient() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = i.applyMetadata(ctx)
		return streamer(ctx, desc, cc, method, opts...)
	}
}

func (i *AuthInterceptor) applyMetadata(ctx context.Context) context.Context {
	kv := make([]string, 0, (len(i.headers)+1)*2)
	if i.tokenSource != nil {
		token, err := i.tokenSource.Token()
		if err != nil {
			slog.Error("failed to get token for stream", "error", err)
		} else {
			kv = append(kv, "authorization", "Bearer "+token.AccessToken)
		}
	}
	for k, v := range i.headers {
		kv = append(kv, k, v)
	}
	if len(kv) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, kv...)
}
