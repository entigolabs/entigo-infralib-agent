package stream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

func dial(ctx context.Context, api *model.NotificationApi, insecureGRPC bool) (*grpc.ClientConn, error) {
	host, pathPrefix, err := parseTarget(api.WrapperURL)
	if err != nil {
		return nil, err
	}
	ts, err := util.GetTokenSource(ctx, api.OAuth)
	if err != nil {
		return nil, err
	}

	interceptors := []grpc.StreamClientInterceptor{
		NewAuthInterceptor(ts, api.Headers).StreamClient(),
	}
	if pathPrefix != "" {
		interceptors = append(interceptors, pathPrefixInterceptor(pathPrefix))
	}
	dialOpts := []grpc.DialOption{
		grpc.WithChainStreamInterceptor(interceptors...),
	}
	if insecureGRPC {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	}

	conn, err := grpc.NewClient(host, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create grpc client: %v", err)
	}
	return conn, nil
}

func parseTarget(raw string) (host, pathPrefix string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("wrapper api url is empty")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("invalid wrapper api url %q: %v", raw, err)
		}
		if u.Host == "" {
			return "", "", fmt.Errorf("wrapper api url %q has no host", raw)
		}
		return u.Host, strings.TrimRight(u.Path, "/"), nil
	}
	if i := strings.Index(raw, "/"); i >= 0 {
		return raw[:i], strings.TrimRight(raw[i:], "/"), nil
	}
	return raw, "", nil
}

func pathPrefixInterceptor(prefix string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(ctx, desc, cc, prefix+method, opts...)
	}
}
