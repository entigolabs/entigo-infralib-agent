package oracle

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
)

// OCI Container Instances have no networkless mode: every instance needs a VNIC
// and every VNIC needs a pre-existing subnet. AWS CodeBuild / GCloud Cloud Run
// treat "no VPC" as a valid state, so their first (network-creating) step runs
// without one; on Oracle that is impossible. The agent therefore owns a bootstrap
// VCN + public subnet, used for any step that does not attach its own VPC. It is
// found-or-created by prefixed name (like the state/config buckets) so it is
// guaranteed ready without persisting an OCID, and reused across executions.
const (
	bootstrapVcnCidr    = "10.0.0.0/16"
	bootstrapSubnetCidr = "10.0.0.0/24"
	networkPollInterval = 5 * time.Second
	networkPollTimeout  = 5 * time.Minute
	anywhereCidr        = "0.0.0.0/0"
)

type Network struct {
	ctx           context.Context
	client        core.VirtualNetworkClient
	compartmentId string
	cloudPrefix   string
}

func NewNetwork(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId, cloudPrefix string) (*Network, error) {
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		client.SetRegion(region)
	}
	return &Network{
		ctx:           ctx,
		client:        client,
		compartmentId: compartmentId,
		cloudPrefix:   cloudPrefix,
	}, nil
}

func (n *Network) vcnName() string    { return fmt.Sprintf("%s-vcn", n.cloudPrefix) }
func (n *Network) igwName() string    { return fmt.Sprintf("%s-igw", n.cloudPrefix) }
func (n *Network) subnetName() string { return fmt.Sprintf("%s-subnet", n.cloudPrefix) }

// EnsureBootstrapSubnet returns the OCID of a public subnet with internet egress,
// creating the VCN, internet gateway, default route and subnet on first call and
// reusing them afterwards. Every resource is matched by its prefixed display name,
// so the function is idempotent across processes and needs no persisted state.
func (n *Network) EnsureBootstrapSubnet() (string, error) {
	vcn, err := n.getOrCreateVcn()
	if err != nil {
		return "", err
	}
	igwId, err := n.getOrCreateInternetGateway(*vcn.Id)
	if err != nil {
		return "", err
	}
	if err = n.ensureDefaultRoute(*vcn.DefaultRouteTableId, igwId); err != nil {
		return "", err
	}
	return n.getOrCreateSubnet(*vcn.Id, *vcn.DefaultRouteTableId, *vcn.DefaultSecurityListId)
}

func (n *Network) getOrCreateVcn() (*core.Vcn, error) {
	name := n.vcnName()
	list, err := n.client.ListVcns(n.ctx, core.ListVcnsRequest{
		CompartmentId: &n.compartmentId,
		DisplayName:   &name,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list vcns: %w", err)
	}
	for _, vcn := range list.Items {
		if vcn.LifecycleState != core.VcnLifecycleStateTerminated && vcn.LifecycleState != core.VcnLifecycleStateTerminating {
			return n.waitForVcn(*vcn.Id)
		}
	}
	cidr := bootstrapVcnCidr
	created, err := n.client.CreateVcn(n.ctx, core.CreateVcnRequest{
		CreateVcnDetails: core.CreateVcnDetails{
			CompartmentId: &n.compartmentId,
			CidrBlock:     &cidr,
			DisplayName:   &name,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create vcn %s: %w", name, err)
	}
	log.Printf("Created bootstrap VCN %s for container execution\n", name)
	return n.waitForVcn(*created.Id)
}

func (n *Network) getOrCreateInternetGateway(vcnId string) (string, error) {
	name := n.igwName()
	list, err := n.client.ListInternetGateways(n.ctx, core.ListInternetGatewaysRequest{
		CompartmentId: &n.compartmentId,
		VcnId:         &vcnId,
		DisplayName:   &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list internet gateways: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].Id, nil
	}
	created, err := n.client.CreateInternetGateway(n.ctx, core.CreateInternetGatewayRequest{
		CreateInternetGatewayDetails: core.CreateInternetGatewayDetails{
			CompartmentId: &n.compartmentId,
			VcnId:         &vcnId,
			IsEnabled:     ocicommon.Bool(true),
			DisplayName:   &name,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create internet gateway %s: %w", name, err)
	}
	return *created.Id, nil
}

// ensureDefaultRoute adds a 0.0.0.0/0 → internet gateway rule to the VCN default
// route table if absent, so the bootstrap subnet reaches the internet (GitHub,
// the container registry, the Object Storage S3-compat endpoint).
func (n *Network) ensureDefaultRoute(routeTableId, igwId string) error {
	rt, err := n.client.GetRouteTable(n.ctx, core.GetRouteTableRequest{RtId: &routeTableId})
	if err != nil {
		return fmt.Errorf("failed to get route table: %w", err)
	}
	for _, rule := range rt.RouteRules {
		if rule.Destination != nil && *rule.Destination == anywhereCidr {
			return nil
		}
	}
	destination := anywhereCidr
	rules := append(rt.RouteRules, core.RouteRule{
		Destination:     &destination,
		DestinationType: core.RouteRuleDestinationTypeCidrBlock,
		NetworkEntityId: &igwId,
	})
	_, err = n.client.UpdateRouteTable(n.ctx, core.UpdateRouteTableRequest{
		RtId:                    &routeTableId,
		UpdateRouteTableDetails: core.UpdateRouteTableDetails{RouteRules: rules},
	})
	if err != nil {
		return fmt.Errorf("failed to add default route: %w", err)
	}
	return nil
}

func (n *Network) getOrCreateSubnet(vcnId, routeTableId, securityListId string) (string, error) {
	name := n.subnetName()
	list, err := n.client.ListSubnets(n.ctx, core.ListSubnetsRequest{
		CompartmentId: &n.compartmentId,
		VcnId:         &vcnId,
		DisplayName:   &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list subnets: %w", err)
	}
	for _, subnet := range list.Items {
		if subnet.LifecycleState != core.SubnetLifecycleStateTerminated && subnet.LifecycleState != core.SubnetLifecycleStateTerminating {
			return n.waitForSubnet(*subnet.Id)
		}
	}
	cidr := bootstrapSubnetCidr
	created, err := n.client.CreateSubnet(n.ctx, core.CreateSubnetRequest{
		CreateSubnetDetails: core.CreateSubnetDetails{
			CompartmentId:   &n.compartmentId,
			VcnId:           &vcnId,
			CidrBlock:       &cidr,
			DisplayName:     &name,
			RouteTableId:    &routeTableId,
			SecurityListIds: []string{securityListId},
			FreeformTags:    map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create subnet %s: %w", name, err)
	}
	log.Printf("Created bootstrap subnet %s for container execution\n", name)
	return n.waitForSubnet(*created.Id)
}

func (n *Network) waitForVcn(vcnId string) (*core.Vcn, error) {
	deadline := time.After(networkPollTimeout)
	for {
		response, err := n.client.GetVcn(n.ctx, core.GetVcnRequest{VcnId: &vcnId})
		if err != nil {
			return nil, fmt.Errorf("failed to get vcn: %w", err)
		}
		if response.LifecycleState == core.VcnLifecycleStateAvailable {
			return &response.Vcn, nil
		}
		select {
		case <-n.ctx.Done():
			return nil, n.ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("timed out waiting for vcn %s to become available", vcnId)
		case <-time.After(networkPollInterval):
		}
	}
}

func (n *Network) waitForSubnet(subnetId string) (string, error) {
	deadline := time.After(networkPollTimeout)
	for {
		response, err := n.client.GetSubnet(n.ctx, core.GetSubnetRequest{SubnetId: &subnetId})
		if err != nil {
			return "", fmt.Errorf("failed to get subnet: %w", err)
		}
		if response.LifecycleState == core.SubnetLifecycleStateAvailable {
			return subnetId, nil
		}
		select {
		case <-n.ctx.Done():
			return "", n.ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("timed out waiting for subnet %s to become available", subnetId)
		case <-time.After(networkPollInterval):
		}
	}
}
