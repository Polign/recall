# The agent-sandbox demo on kind. Needs docker, kind, kubectl and helm.
#
#   make demo      create the cluster, install everything, run the demo
#   make clean     delete the cluster
#
# The recall image is built from this repository and loaded into kind, so
# nothing here needs a published release.

CLUSTER ?= recall-sandbox
AGENT_SANDBOX_VERSION ?= v1.0.6
IMAGE ?= ghcr.io/polign/recall:dev
POLIGN_CHART ?= oci://ghcr.io/polign/charts/polign
# The recall backend needs polign_db 0.8.0 or later; the chart's default image is older.
POLIGN_VERSION ?= 0.14.0
RECALL_CHART ?= ../../../polign/charts/recall
KUBECTL = kubectl --context kind-$(CLUSTER)
HELM = helm --kube-context kind-$(CLUSTER)

.PHONY: demo cluster image store server sandboxes run clean

demo: cluster image store server sandboxes run

cluster:
	kind get clusters | grep -qx $(CLUSTER) || kind create cluster --name $(CLUSTER) --wait 120s
	$(KUBECTL) apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/sandbox-with-extensions.yaml
	$(KUBECTL) -n agent-sandbox-system wait --for=condition=Available deployment --all --timeout=180s 2>/dev/null || true

image:
	docker build -f ../container/Dockerfile --build-arg VERSION=$$(git describe --tags --always) -t $(IMAGE) ../..
	kind load docker-image $(IMAGE) --name $(CLUSTER)

# polign_db on a local volume: the quickest store that exists. A real
# install points store.uri at a bucket.
store:
	$(KUBECTL) create namespace polign --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n polign get secret polign-key >/dev/null 2>&1 || \
	  $(KUBECTL) -n polign create secret generic polign-key \
	    --from-literal=api-key="$$(docker run --rm --entrypoint /usr/local/bin/polign-apikey ghcr.io/polign/polign-server:$(POLIGN_VERSION) generate)"
	$(HELM) upgrade --install polign $(POLIGN_CHART) -n polign \
	  --set store.claim.create=true --set auth.existingSecret=polign-key --set image.tag=$(POLIGN_VERSION) --wait --timeout 5m

server:
	$(KUBECTL) create namespace recall --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n recall get secret polign-key >/dev/null 2>&1 || \
	  $(KUBECTL) -n recall create secret generic polign-key \
	    --from-literal=api-key="$$($(KUBECTL) -n polign get secret polign-key -o jsonpath='{.data.api-key}' | base64 -d)"
	$(KUBECTL) -n recall get secret recall-admin >/dev/null 2>&1 || \
	  $(KUBECTL) -n recall create secret generic recall-admin --from-literal=key="rcl_$$(openssl rand -hex 8)_$$(openssl rand -base64 24 | tr '+/' '-_' | tr -d =)"
	$(HELM) upgrade --install recall $(RECALL_CHART) -n recall -f recall-values.yaml \
	  --set image.repository=$$(echo $(IMAGE) | cut -d: -f1) --set image.tag=$$(echo $(IMAGE) | cut -d: -f2) \
	  --set image.pullPolicy=Never --wait --timeout 3m

sandboxes:
	$(KUBECTL) apply -f namespace.yaml
	$(KUBECTL) apply -f sandbox.yaml
	$(KUBECTL) apply -f template.yaml
	$(KUBECTL) -n agents wait --for=condition=Ready pod/demo-1 --timeout=180s

run:
	$(KUBECTL) -n agents exec demo-1 -- sh -s < demo.sh

clean:
	kind delete cluster --name $(CLUSTER)
