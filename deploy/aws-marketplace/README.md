# AWS Marketplace image

Packer build for the Recall AMI and the CloudFormation template that ships
with the listing. The image is Amazon Linux 2023 with Recall, polign_db as its
store on the same instance, and a first-boot script that configures both from
the launch. It holds no credentials and no bucket name.

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
cd deploy/aws-marketplace
packer init .
AWS_PROFILE=polign packer build -var recall_version=v0.22.0 -var polign_version=v0.15.0 .
```

Build in the Marketplace seller account (298123941500, profile `polign`).
The build checks the account first and stops if it is a different one.
`-var arch=amd64` builds an x86 image; the default is arm64 (Graviton).
The build downloads both releases by tag and the model files from Hugging
Face, each checked against a pinned checksum. Nothing is downloaded at boot.
The model revision and hashes in `scripts/install.sh` match
`model/local.go`; change both together.

## Launch

`cloudformation/recall.yaml` creates the bucket (or takes yours), an
instance role scoped to it, a security group open on 8080 to the CIDR you
name, and optionally an internal NLB. The outputs give the endpoint, a
Session Manager command, and the command that prints the admin key.

```sh
aws cloudformation deploy --template-file cloudformation/recall.yaml --stack-name recall-test \
  --capabilities CAPABILITY_IAM --parameter-overrides ImageId=ami-... VpcId=vpc-... \
  SubnetIds=subnet-... AllowedCidr=10.0.0.0/16
```

A launch without the template supplies the store as the instance tag
`recall:store = s3://bucket/prefix` (with instance metadata tags enabled) or
as `POLIGN_STORE` in `/etc/recall/recall.env` through cloud-init.
