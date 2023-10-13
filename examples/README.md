== Example client configurations

These are example configruations on the client side.

These will work withoyt any modification in the entigo-infralib account. If you want to use your own account then change the DNS configruation accordingly.


== To remove all created resources

1) Delete all PV and PVC resources
2) Delete all ingress resources
3) Run helm-destroy
4) Remove all route53 domains created by external-dns
5) Run infra-destroy
6) Run net-destroy
7) Remove agent resources
