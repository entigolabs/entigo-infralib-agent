package oracle

import (
	"context"
	"fmt"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
)

type oracleProvider struct {
	ctx           context.Context
	cloudPrefix   string
	compartmentId string
	region        string
	provider      ocicommon.ConfigurationProvider
}

func NewOracleProvider(ctx context.Context, oracle common.Oracle, cloudPrefix string) (model.ResourceProvider, error) {
	provider, err := newConfigProvider()
	if err != nil {
		return nil, err
	}
	return &oracleProvider{
		ctx:           ctx,
		cloudPrefix:   cloudPrefix,
		compartmentId: oracle.CompartmentId,
		region:        oracle.Region,
		provider:      provider,
	}, nil
}

func (o *oracleProvider) GetProviderType() model.ProviderType {
	return model.ORACLE
}

func (o *oracleProvider) GetSSM() (model.SSM, error) {
	configStorage, err := NewStorage(o.ctx, o.provider, o.region, o.compartmentId,
		getConfigBucketName(o.cloudPrefix, o.region))
	if err != nil {
		return nil, fmt.Errorf("failed to create config storage service: %w", err)
	}
	return NewSSM(configStorage), nil
}

func (o *oracleProvider) GetBucket(prefix string) (model.Bucket, error) {
	bucket := getBucketName(prefix, o.region)
	storage, err := NewStorage(o.ctx, o.provider, o.region, o.compartmentId, bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to create object storage service: %w", err)
	}
	return storage, nil
}
