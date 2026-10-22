# Google Cloud Marketplace image

Packer build for the Recall Compute Engine image and the Terraform module that
ships with the listing. The image is Debian 12 with Recall, polign_db as its
store on the same VM, and a first-boot script that configures both from the
launch. It holds no credentials and no bucket name.

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

## Build

```sh
cd deploy/gcp-marketplace
packer init .
gcloud config set project polign-admin
packer build -var project_id=polign-admin -var recall_version=v0.22.1 -var polign_version=v0.15.0 .
```

The image goes into the producer project, and the build stops if the active
gcloud project is a different one. The default architecture is amd64, because
Arm machine types are offered in few zones. A Marketplace build also needs
`-var 'image_licenses=["projects/<producer>/global/licenses/<id>"]'`, which
Producer Portal issues; a license cannot be added to an existing image.

## Launch

`terraform/` creates the bucket (or takes yours), a service account granted
only that bucket, a firewall rule open on 8080 to the ranges you name, and SSH
over IAP. Settings reach first boot as metadata attributes (`recall-store`,
`recall-embed-model`, `recall-disk-cache-bytes`, `recall-extra-args`).

```sh
cd terraform
terraform init
terraform apply -var project_id=my-project -var image_project=polign-admin \
  -var 'allowed_cidrs=["10.128.0.0/9"]'
```

The outputs give the endpoint and the command that prints the admin key.
