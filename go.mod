module github.com/entigolabs/entigo-infralib-agent

go 1.24.0

toolchain go1.24.5

require (
	cloud.google.com/go/deploy v1.27.3
	cloud.google.com/go/logging v1.13.1
	cloud.google.com/go/run v1.12.1
	cloud.google.com/go/scheduler v1.11.8
	cloud.google.com/go/secretmanager v1.16.0
	cloud.google.com/go/storage v1.57.1
	dario.cat/mergo v1.0.2
	github.com/atc0005/go-teams-notify/v2 v2.13.0
	github.com/aws/aws-sdk-go-v2 v1.39.6
	github.com/aws/aws-sdk-go-v2/config v1.31.18
	github.com/aws/aws-sdk-go-v2/credentials v1.18.22
	github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs v1.58.7
	github.com/aws/aws-sdk-go-v2/service/codebuild v1.68.2
	github.com/aws/aws-sdk-go-v2/service/codepipeline v1.46.10
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.52.4
	github.com/aws/aws-sdk-go-v2/service/iam v1.50.0
	github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi v1.30.11
	github.com/aws/aws-sdk-go-v2/service/s3 v1.90.0
	github.com/aws/aws-sdk-go-v2/service/scheduler v1.17.10
	github.com/aws/aws-sdk-go-v2/service/secretsmanager v1.39.11
	github.com/aws/aws-sdk-go-v2/service/ssm v1.67.0
	github.com/aws/aws-sdk-go-v2/service/sts v1.40.0
	github.com/aws/smithy-go v1.23.2
	github.com/brianvoe/gofakeit/v6 v6.28.0
	github.com/go-git/go-billy/v5 v5.6.2
	github.com/go-git/go-git/v5 v5.16.3
	github.com/google/uuid v1.6.0
	github.com/googleapis/gax-go/v2 v2.15.0
	github.com/hashicorp/go-version v1.7.0
	github.com/hashicorp/hcl/v2 v2.24.0
	github.com/slack-go/slack v0.17.3
	github.com/urfave/cli/v3 v3.6.0
	github.com/zclconf/go-cty v1.17.0
	golang.org/x/crypto v0.43.0
	golang.org/x/text v0.30.0
	google.golang.org/api v0.255.0
	google.golang.org/genproto v0.0.0-20251110190251-83f479183930
	google.golang.org/genproto/googleapis/api v0.0.0-20251110190251-83f479183930
	google.golang.org/grpc v1.76.0
	google.golang.org/protobuf v1.36.10
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cel.dev/expr v0.25.1 // indirect
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.17.0 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.2.8 // indirect
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	cloud.google.com/go/iam v1.5.3 // indirect
	cloud.google.com/go/longrunning v0.7.0 // indirect
	cloud.google.com/go/monitoring v1.24.3 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/detectors/gcp v1.30.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric v0.54.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/internal/resourcemapping v0.54.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/ProtonMail/go-crypto v1.3.0 // indirect
	github.com/agext/levenshtein v1.2.3 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.3 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.13 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.13 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.13 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.11.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.5 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.6.1 // indirect
	github.com/cncf/xds/go v0.0.0-20251110193048-8bfbf64dc13e // indirect
	github.com/cyphar/filepath-securejoin v0.6.0 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/envoyproxy/go-control-plane/envoy v1.36.0 // indirect
	github.com/envoyproxy/protoc-gen-validate v1.2.1 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-jose/go-jose/v4 v4.1.3 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/s2a-go v0.1.9 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.7 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/kevinburke/ssh_config v1.4.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/pjbgf/sha1cd v0.5.0 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/skeema/knownhosts v1.3.2 // indirect
	github.com/spiffe/go-spiffe/v2 v2.6.0 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/detectors/gcp v1.38.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.63.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.63.0 // indirect
	go.opentelemetry.io/otel v1.38.0 // indirect
	go.opentelemetry.io/otel/metric v1.38.0 // indirect
	go.opentelemetry.io/otel/sdk v1.38.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.38.0 // indirect
	go.opentelemetry.io/otel/trace v1.38.0 // indirect
	golang.org/x/mod v0.29.0 // indirect
	golang.org/x/net v0.46.0 // indirect
	golang.org/x/oauth2 v0.33.0 // indirect
	golang.org/x/sync v0.18.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	golang.org/x/tools v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251110190251-83f479183930 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
)
