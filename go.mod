module github.com/entigolabs/entigo-infralib-agent

go 1.23

exclude google.golang.org/grpc v1.69.0 // Avoid https://github.com/googleapis/google-cloud-go/issues/11283

require (
	cloud.google.com/go/deploy v1.26.0
	cloud.google.com/go/logging v1.12.0
	cloud.google.com/go/run v1.8.0
	cloud.google.com/go/secretmanager v1.14.2
	cloud.google.com/go/storage v1.48.0
	dario.cat/mergo v1.0.1
	github.com/aws/aws-sdk-go-v2 v1.33.0
	github.com/aws/aws-sdk-go-v2/config v1.28.6
	github.com/aws/aws-sdk-go-v2/credentials v1.17.47
	github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs v1.45.0
	github.com/aws/aws-sdk-go-v2/service/codebuild v1.49.3
	github.com/aws/aws-sdk-go-v2/service/codepipeline v1.37.1
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.38.0
	github.com/aws/aws-sdk-go-v2/service/iam v1.38.2
	github.com/aws/aws-sdk-go-v2/service/s3 v1.73.2
	github.com/aws/aws-sdk-go-v2/service/ssm v1.56.1
	github.com/aws/aws-sdk-go-v2/service/sts v1.33.2
	github.com/aws/smithy-go v1.22.1
	github.com/brianvoe/gofakeit/v6 v6.28.0
	github.com/go-git/go-git/v5 v5.12.0
	github.com/google/go-github/v60 v60.0.0
	github.com/google/uuid v1.6.0
	github.com/googleapis/gax-go/v2 v2.14.0
	github.com/hashicorp/go-version v1.7.0
	github.com/hashicorp/hcl/v2 v2.23.0
	github.com/urfave/cli/v2 v2.27.5
	github.com/zclconf/go-cty v1.15.1
	golang.org/x/crypto v0.31.0
	golang.org/x/text v0.21.0
	google.golang.org/api v0.212.0
	google.golang.org/genproto/googleapis/api v0.0.0-20241216192217-9240e9c98484
	google.golang.org/grpc v1.68.1
	google.golang.org/protobuf v1.36.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cel.dev/expr v0.19.1 // indirect
	cloud.google.com/go v0.117.0 // indirect
	cloud.google.com/go/auth v0.13.0 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.2.6 // indirect
	cloud.google.com/go/compute/metadata v0.6.0 // indirect
	cloud.google.com/go/iam v1.3.0 // indirect
	cloud.google.com/go/longrunning v0.6.3 // indirect
	cloud.google.com/go/monitoring v1.22.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/detectors/gcp v1.25.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric v0.49.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/internal/resourcemapping v0.49.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/ProtonMail/go-crypto v1.1.3 // indirect
	github.com/agext/levenshtein v1.2.3 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.6.7 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.16.21 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.3.28 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.6.28 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.1 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.3.28 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.12.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.5.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.10.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.12.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.18.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.24.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.28.6 // indirect
	github.com/census-instrumentation/opencensus-proto v0.4.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.5.0 // indirect
	github.com/cncf/xds/go v0.0.0-20241213214725-57cfbe6fad57 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.6 // indirect
	github.com/cyphar/filepath-securejoin v0.3.6 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/envoyproxy/go-control-plane v0.13.1 // indirect
	github.com/envoyproxy/protoc-gen-validate v1.1.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-git/go-billy/v5 v5.6.0 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/go-cmp v0.6.0 // indirect
	github.com/google/go-querystring v1.1.0 // indirect
	github.com/google/s2a-go v0.1.8 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.4 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/kevinburke/ssh_config v1.2.0 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/pjbgf/sha1cd v0.3.0 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/sergi/go-diff v1.3.2-0.20230802210424-5b0b94c5c0d3 // indirect
	github.com/skeema/knownhosts v1.3.0 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	github.com/xrash/smetrics v0.0.0-20240521201337-686a1a2994c1 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/contrib/detectors/gcp v1.33.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.58.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.58.0 // indirect
	go.opentelemetry.io/otel v1.33.0 // indirect
	go.opentelemetry.io/otel/metric v1.33.0 // indirect
	go.opentelemetry.io/otel/sdk v1.33.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.33.0 // indirect
	go.opentelemetry.io/otel/trace v1.33.0 // indirect
	golang.org/x/mod v0.22.0 // indirect
	golang.org/x/net v0.32.0 // indirect
	golang.org/x/oauth2 v0.24.0 // indirect
	golang.org/x/sync v0.10.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/time v0.8.0 // indirect
	golang.org/x/tools v0.28.0 // indirect
	google.golang.org/genproto v0.0.0-20241216192217-9240e9c98484 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241216192217-9240e9c98484 // indirect
	google.golang.org/grpc/stats/opentelemetry v0.0.0-20241028142157-ada6787961b3 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
)
