package service

import (
	"github.com/entigolabs/entigo-infralib-agent/aws"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"strings"
)

//ctx := context.Background()
//client, err := storage.NewClient(ctx)
//if err != nil {
//	common.PrintError(err)
//} else {
//	defer func(client *storage.Client) {
//		_ = client.Close()
//	}(client)
//}
//it := client.Buckets(ctx, "mart-test-425506")
//for {
//	bucketAttrs, err := it.Next()
//	if errors.Is(err, iterator.Done) {
//		break
//	}
//	if err != nil {
//		common.PrintError(err)
//		break
//	}
//	fmt.Println(bucketAttrs.Name)
//}

func GetCloudProvider(flags *common.Flags) model.CloudProvider {
	prefix := GetAwsPrefix(flags)
	cfg, err := aws.GetAWSConfig()
	if err == nil {
		return aws.NewAWS(strings.ToLower(prefix), *cfg)
	} else {
		// Try to initialize google cloud resources
		common.Logger.Fatalf("Failed to initialize cloud provider: %s", err)
	}
	return nil
}
