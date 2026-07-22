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
	kms, err := NewKMS(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create kms service: %w", err)
	}
	if err = kms.Ensure(); err != nil {
		return nil, fmt.Errorf("failed to provision kms vault and key: %w", err)
	}
	ssm, err := NewSSM(o.ctx, o.provider, o.region, o.compartmentId, kms.VaultId(), kms.KeyId())
	if err != nil {
		return nil, fmt.Errorf("failed to create secret store: %w", err)
	}
	return ssm, nil
}

func (o *oracleProvider) GetBucket(prefix string) (model.Bucket, error) {
	bucket := getBucketName(prefix, o.region)
	storage, err := NewStorage(o.ctx, o.provider, o.region, o.compartmentId, bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to create object storage service: %w", err)
	}
	return storage, nil
}
