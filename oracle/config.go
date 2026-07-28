package oracle

import (
	"fmt"
	"os"

	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
)

// newConfigProvider resolves OCI credentials like the oci CLI: resource principal
// in-container (signalled by the env var), else the SDK default chain
// (~/.oci/config or OCI_CONFIG_FILE). Region is applied per client via SetRegion.
func newConfigProvider() (ocicommon.ConfigurationProvider, error) {
	if os.Getenv(auth.ResourcePrincipalRPSTEnvVar) != "" {
		return auth.ResourcePrincipalConfigurationProvider()
	}
	return ocicommon.DefaultConfigProvider(), nil
}

func getBucketName(cloudPrefix, region string) string {
	return fmt.Sprintf("%s-%s", cloudPrefix, region)
}

// s3Endpoint is the S3-compatible Object Storage endpoint used by the terraform
// s3 backend, e.g. https://<namespace>.compat.objectstorage.<region>.oraclecloud.com
func s3Endpoint(namespace, region string) string {
	return fmt.Sprintf("https://%s.compat.objectstorage.%s.oraclecloud.com", namespace, region)
}
