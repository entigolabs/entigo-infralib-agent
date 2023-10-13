## Example client configurations

These are example configruations on the client side.

These will work withoyt any modification in the entigo-infralib account. If you want to use your own account then change the DNS configruation accordingly.

## Example usage 

```
export AWS_ACCESS_KEY_ID=
export AWS_SECRET_ACCESS_KEY=
export AWS_SESSION_TOKEN=
export AWS_REGION="eu-north-1"

#Pri With bootstrap
docker run --pull always -it --rm -v "$(pwd)/examples/pri.yaml":"/etc/ei-agent/config.yaml" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent ei-agent bootstrap
#Biz Without bootstrap
docker run --pull always -it --rm -v "$(pwd)/examples/biz.yaml":"/etc/ei-agent/config.yaml" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent
```

## To remove all created resources
Run removal pipelines in reverse order as they appear in the merged configuration. Remember to remove EBS, ALB, NLB resources and Route53 records before doing that.

For example:
1) Enable all the destroy pipeline transitions
2) Delete all PV and PVC resources from the EKS cluster
3) Delete all ingress resources from the EKS cluster
4) Run helm-destroy
5) Remove all route53 domains created by external-dns
6) Run infra-destroy
7) Run net-destroy
8) Remove agent resources
