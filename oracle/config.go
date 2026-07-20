package oracle

import (
	"fmt"
	"os"

	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
)

// newConfigProvider builds an OCI SDK configuration provider, mirroring how the
// oci CLI resolves credentials. In-container runs (OCI Container Instances)
// authenticate via resource principal, signalled by the injected version env var;
// every other run uses the SDK default chain — ~/.oci/config (or the path in
// OCI_CONFIG_FILE) plus config env vars. Region is applied separately via
// SetRegion on each client, so a resource-principal run needs no region here.
func newConfigProvider() (ocicommon.ConfigurationProvider, error) {
	if os.Getenv(auth.ResourcePrincipalVersionEnvVar) != "" {
		return auth.ResourcePrincipalConfigurationProvider()
	}
	return ocicommon.DefaultConfigProvider(), nil
}

// getBucketName is the terraform-state bucket. Object Storage bucket names are
// unique within the tenancy namespace, so the deployment prefix + region is
// enough to disambiguate parallel deployments.
func getBucketName(cloudPrefix, region string) string {
	return fmt.Sprintf("%s-%s", cloudPrefix, region)
}

// getConfigBucketName holds agent parameters/secrets (the SSM equivalent),
// kept separate from terraform state so their lifecycles don't interfere.
func getConfigBucketName(cloudPrefix, region string) string {
	return fmt.Sprintf("%s-%s-config", cloudPrefix, region)
}

// s3Endpoint is the S3-compatible Object Storage endpoint used by the terraform
// s3 backend, e.g. https://<namespace>.compat.objectstorage.<region>.oraclecloud.com
func s3Endpoint(namespace, region string) string {
	return fmt.Sprintf("https://%s.compat.objectstorage.%s.oraclecloud.com", namespace, region)
}
