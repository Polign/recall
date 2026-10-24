# Azure Marketplace image

Packer build for the Recall VM image listed on Azure Marketplace. The image is
Debian 12 Generation 2 with Recall, polign_db as its store on the same VM, and
a first-boot script that configures both from the launch. It holds no
credentials and no storage account name; the VM's managed identity is how the
database reaches its container.

## What is on the image

| Piece | Where | Notes |
| --- | --- | --- |
| `recall` | `/usr/local/bin/recall`, unit `recall.service` | `recall serve` on `:8080`, the only port the instance serves |
| `polign-server` | `/usr/local/bin/polign-server`, unit `polign-server.service` | The store, on `127.0.0.1:23000` only, with a data key |
| Embedding model | `/opt/recall/models/all-minilm-l6-v2` | Search by meaning with no API key and no outbound call |
| First boot | `/usr/local/sbin/recall-firstboot` | Resolves the store, writes `/etc/recall/recall.env`, generates the keys |

First boot generates two keys on the instance, so no two launches share one:

- `/etc/recall/polign-key`: the key Recall uses to reach the database on
  localhost. Nothing else needs it.
- `/etc/recall/admin-key`: the first Recall admin key. Use it to mint a key
  per application or tenant:

```sh
curl -s -H "Authorization: Bearer $(sudo cat /etc/recall/admin-key)" \
  -d '{"namespace":"acme"}' http://localhost:8080/v1/keys
```

The returned `key` reaches only its namespace. Clients use it against
`http://<host>:8080/v1/...` or the MCP endpoint `http://<host>:8080/mcp`.

Settings live in `/etc/recall/recall.env`; edit it and run
`systemctl restart polign-server recall`. Anything a launch puts there before
first boot wins over the defaults, including API keys for a hosted embedder
or extraction model (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`). Choose the
embedder before the first write: memory is searched with the embedder it was
written with.

Recall serves plain HTTP. Keep the port inside your network, or put a TLS
load balancer in front.

## Prerequisites

The build publishes a version into the existing `polign_marketplace` gallery
(subscription `a19d6e8f-f315-4df4-be3e-6c4659ce0ba8`, resource group
`polign-marketplace`, westus2). Recall needs its own image definition, created
once, with the features set right at creation (they cannot be changed after a
version exists, and the v6/v7 sizes are NVMe-only):

```sh
az sig image-definition create -g polign-marketplace --gallery-name polign_marketplace \
  --gallery-image-definition recall-x64 --publisher Polign --offer recall --sku x64-gen2 \
  --os-type Linux --os-state Generalized --hyper-v-generation V2 --architecture x64 \
  --features "SecurityType=TrustedLaunchSupported DiskControllerTypes=SCSI,NVMe IsAcceleratedNetworkSupported=True"
```

Partner Center reads the gallery through the two reader grants already on it,
so a new definition needs no new grants.

## Build

```sh
cd deploy/azure-marketplace
packer init .
az account set --subscription a19d6e8f-f315-4df4-be3e-6c4659ce0ba8
packer build -var recall_version=v0.23.0 -var polign_version=v0.15.0 .
```

The gallery version is the Recall version (v0.23.0 publishes 0.22.1). A
version cannot be overwritten, so a rebuild of the same release needs
`-var gallery_version=0.22.2`.

## Launch

Set the VM tag `recall-store = az://<account>/<container>/<prefix>`, turn on
the system-assigned identity, give it Storage Blob Data Contributor on the
container, and open 8080 to your virtual network in the network security
group. Tags `recall-embed-model`, `recall-disk-cache-bytes` and
`recall-extra-args` set the matching values. The database restarts every
2 seconds until the role assignment propagates, which is expected.
