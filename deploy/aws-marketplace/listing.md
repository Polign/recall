# AWS Marketplace listing: Recall

Copy for every field the Marketplace Management Portal asks for when creating
the Recall server product. Paste each block as written. The product is new and
separate from the polign_db listing.

## Product information

**Product title**

Recall by Polign

**SKU** (optional)

RECALL-AMI

**Short description**

Recall is agent memory that keeps up when facts change. A newer statement replaces the older one, the replaced fact stays on record, and every memory lives in an S3 bucket in your own account.

**Long description**

```
Recall gives AI agents a memory that stays correct as facts change. When a user moves, switches teams, or changes a preference, Recall replaces the old belief with the new one instead of returning both, and keeps the replaced one on record so you can see what was believed and when.

This AMI runs Recall on a single EC2 instance in your account:

- Recall listens on port 8080 with an HTTP API and an MCP endpoint, so agents built on Claude, OpenAI, LangGraph, or any MCP client can use it directly.
- polign_db stores the memory on the same instance and writes it to an S3 bucket you own. The bucket is the system of record, so replacing the instance loses nothing.
- Search by meaning works out of the box with an embedding model built into the image. No API key is needed and nothing is sent outside your account. Hosted embedders (OpenAI, Voyage) and an optional extraction model can be configured instead.
- Every request needs a Recall key. Each key reaches one namespace, so one deployment can serve many applications or tenants with their memory kept apart.

The CloudFormation template that ships with the product creates the bucket, an instance role scoped to that bucket, and a security group scoped to your VPC. First boot generates the instance's own keys; the image ships with none.
```

**Highlights** (three)

1. `Memory that keeps up with change: a newer statement replaces the older one, and the replaced fact stays on record for history and audit.`
2. `Your account, your bucket: memory is stored in an S3 bucket you own, reached through an instance role. No data or keys leave your AWS account.`
3. `Works with your agents: HTTP API and MCP endpoint on port 8080, with per-namespace keys so one deployment serves many apps or tenants.`

**Product categories**

1. Machine Learning
2. Databases & Analytics Platforms
3. Storage

**Search keywords**

agent memory, AI agent memory, MCP, LLM memory, long-term memory, RAG, Claude, LangGraph, semantic search, S3

**Product logo**

`polign_web/assets/brand/icon-512-transparent.png` (PNG, at least 110x110, transparent background).

**Product video / additional resources** (optional)

- Documentation: https://polign.com/docs
- Source code: https://github.com/Polign/recall

**Support information**

```
Email support@polign.com with the region, the instance id, and the output of recall --version. We help with deployment, configuration of the store and embedder, key management, MCP and HTTP integration, and upgrades.

For billing questions about your AWS Marketplace subscription, contact AWS Marketplace support.
```

## Regions

us-east-1 and us-west-1, the same as polign_db. Leave "future regions" off.

## Version 0.22.1

**Version title**

0.22.1

**Release notes**

```
Recall 0.22.1 on polign_db 0.15.0 (arm64, Amazon Linux 2023).

First release of the Recall AMI. Recall listens on port 8080; polign_db runs on the same instance on localhost only and stores memory in an S3 bucket you own. The image includes the all-MiniLM-L6-v2 embedding model, so search by meaning works without an API key or outbound access.

polign_db sends an anonymous heartbeat at startup and every ten minutes: version, OS and architecture, a random install id, and aggregate counts. It sends no bucket names, keys, namespace names, or memory content. To opt out, set POLIGN_EXTRA_ARGS=-telemetry=false in /etc/recall/recall.env and run sudo systemctl restart polign-server.
```

**AMI ID**

ami-00f6415e51391bede

**IAM access role ARN**

arn:aws:iam::298123941500:role/polign-marketplace-ami-ingestion

**Operating system**: Amazon Linux, version 2023

**OS user name**: ec2-user

**Scanning port**: 22

**Usage instructions** (AMI)

```
Recall runs on one EC2 instance and stores memory in an S3 bucket you own. The image ships with no credentials and no bucket name, so a launch supplies the bucket. The CloudFormation template that comes with this product does everything below for you.

CONFIGURE THE STORE
Either works:
- Instance tag recall:store = s3://your-bucket/recall, with instance metadata tags enabled.
- Or /etc/recall/recall.env written before first boot (cloud-init write_files) containing POLIGN_STORE=s3://your-bucket/recall

The bucket must already exist. Attach an instance role granting s3:GetObject, s3:PutObject, s3:DeleteObject, s3:AbortMultipartUpload, s3:ListMultipartUploadParts on arn:aws:s3:::your-bucket/*, and s3:ListBucket, s3:GetBucketLocation, s3:ListBucketMultipartUploads on arn:aws:s3:::your-bucket. Add AmazonSSMManagedInstanceCore for shell access without an inbound port.

SECURITY GROUP
Open port 8080 to your VPC CIDR only. Every request needs a Recall key; the database listens on localhost and is not reachable from the network.

GET A KEY
First boot writes the instance's admin key to /etc/recall/admin-key. Over Session Manager:

ADMIN=$(sudo cat /etc/recall/admin-key)
curl -s -H "Authorization: Bearer $ADMIN" -d '{"namespace":"my-app"}' http://localhost:8080/v1/keys

The returned key reaches only the my-app namespace. Store it in your secret manager.

USE IT
From a host inside your VPC, with KEY set to that key:

curl -H "Authorization: Bearer $KEY" -d '{"text":"The customer prefers email over phone calls."}' http://INSTANCE-IP:8080/v1/remember
curl -H "Authorization: Bearer $KEY" -d '{"question":"How should we contact the customer?"}' http://INSTANCE-IP:8080/v1/recall

MCP clients connect to http://INSTANCE-IP:8080/mcp with the same key as a bearer token.

OPERATE
Settings live in /etc/recall/recall.env; edit and run sudo systemctl restart polign-server recall. Logs: journalctl -u recall and journalctl -u polign-server. polign_db sends an anonymous heartbeat; set POLIGN_EXTRA_ARGS=-telemetry=false to opt out.

SUPPORT
Email support@polign.com with the region, the instance id, and the output of recall --version.
```

**Recommended instance type**: m7g.large

**Security group recommendations**

| Protocol | Port | CIDR |
| --- | --- | --- |
| TCP | 8080 | 10.0.0.0/16 (replace with your VPC CIDR) |

## Pricing

Free (no software charge), the same as polign_db on 0.x. Instance types: t4g.medium, m7g.large, m7g.xlarge, m7g.2xlarge, r7g.large, r7g.xlarge, r7g.2xlarge.

## Refund policy

```
Recall is offered at no software charge on AWS Marketplace, so there is nothing to refund. AWS infrastructure charges are billed by AWS. Contact support@polign.com with any question.
```

## EULA

Standard Contract for AWS Marketplace (SCMP), the same as polign_db.

## Allowlist (limited visibility)

298123941500 (the seller account), so the listing can be launch-tested before requesting Public.
