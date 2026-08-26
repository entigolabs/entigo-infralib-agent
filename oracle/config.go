package oracle

import (
	"fmt"
	"os"

	"github.com/entigolabs/entigo-infralib-agent/common"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
)

const defaultProfile = "DEFAULT"

// newConfigProvider resolves OCI credentials like the oci CLI: resource principal
// in-container (signalled by the env var), else the SDK default chain
// (~/.oci/config or OCI_CONFIG_FILE). A config file or profile given by flag narrows
// that to the named file and section, falling back to the DEFAULT section for keys the
// profile omits. Region is applied per client via SetRegion.
func newConfigProvider(oracle common.Oracle) (ocicommon.ConfigurationProvider, error) {
	if os.Getenv(auth.ResourcePrincipalRPSTEnvVar) != "" {
		return auth.ResourcePrincipalConfigurationProvider()
	}
	if oracle.ConfigFile == "" && oracle.Profile == "" {
		return ocicommon.DefaultConfigProvider(), nil
	}
	profile := oracle.Profile
	if profile == "" {
		profile = defaultProfile
	}
	return ocicommon.CustomProfileConfigProvider(oracle.ConfigFile, profile), nil
}

func getBucketName(cloudPrefix, region string) string {
	return fmt.Sprintf("%s-%s", cloudPrefix, region)
}

// s3Endpoint is the S3-compatible Object Storage endpoint used by the terraform
// s3 backend, e.g. https://<namespace>.compat.objectstorage.<region>.oraclecloud.com
func s3Endpoint(namespace, region string) string {
	return fmt.Sprintf("https://%s.compat.objectstorage.%s.oraclecloud.com", namespace, region)
}
