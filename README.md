# ControllerMesh

ControllerMesh is a solution that helps developers manage their controllers/operators better.

## Key Features

1. Canary update: the controllers can be updated in canary progress instead of one time replace.
2. Fault injection: it helps developers to verify their reconcile logic in some fault scenarios.
3. Flexible isolation: limits resources of which namespaces can be queried by a controller.
4. Client-side rate-limit and blown.

## Implementation Constraints

Generally, a `ctrlmesh-proxy` container will be injected into each operator Pod that has configured in ControllerMesh.
This proxy container will intercept and handle the connection by between API Server and controllers/webhooks in the Pod.

<p align="center"><img width="500" src="./docs/img/readme-1.png"/></p>

The `ctrlmesh-manager` dispatches rules to the proxies, so that they can route requests according to the rules.

<p align="center"><img width="500" src="./docs/img/readme-2.png"/></p>

A core CRD in ControllerMesh is `VirtualApp`. It contains all rules for user's controller and webhook:

```yaml
apiVersion: ctrlmesh.kruise.io/v1alpha1
kind: VirtualApp
metadata:
  name: test-operator
  # ...
spec:
  selector:
    matchLabels:
      component: test-operator
  configuration:
    controller:
      leaderElection:
        lockName: test-operator
    webhook:
      certDir: /tmp/webhook-certs
      port: 9443
  route:
    globalLimits:
    - namespaceSelector:
        matchExpressions:
        - key: ns-type
          operator: NotIn
          values:
          - system
    subRules:
    - name: canary-rule
      match:
      - namespaceSelector:
          matchLabels:
            ns-type: canary-1
      - namespaceRegex: "^canary.*"
  subsets:
  - name: v2
    labels:
      version: v2
    routeRules:
    - canary-rule
```

- selector: for all pods of the test-operator
- configuration:
  - controller: configuration for controller, including leader election name
  - webhook: configuration for webhook, including certDir and port of this webhook
- route:
  - globalLimits: limit rules that enable to all pods of test-operator
  - subRules: multiple rules that can define to be used in subsets
- subsets: multiple groups of the pods, each subset has specific labels and its route rules

### Flow control

ControllerMesh will firstly support **Hard Limit** type of flow control,
which means the ctrlmesh-proxy will filter unmatched requests/responses between API Server and local controller/webhook.

Controller:

<p align="center"><img width="500" src="./docs/img/readme-3.png"/></p>

Webhook:

<p align="center"><img width="500" src="./docs/img/readme-4.png"/></p>

### Risks and Mitigations

1. The controller/webhook can not get any requests if ctrlmesh-proxy container crashes.
2. Developers can not change the flow rules of their operators if kruise-manager is not working.
3. The performance of controller/webhook will be a little worse.
4. Pod of the operator requires a few more resources because of a ctrlmesh-proxy container injected into it.

## License

ControllerMesh is licensed under the Apache License, Version 2.0. See [LICENSE](./LICENSE.md) for the full license text.

