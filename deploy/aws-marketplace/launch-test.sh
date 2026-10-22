#!/bin/bash
# Launch test for a built AMI: creates the listing's CloudFormation stack,
# then checks the instance over Session Manager (healthz, a minted namespace
# key, remember and recall by meaning, a 401 without a key).
#
#   AWS_PROFILE=polign ./launch-test.sh <ami-id> <vpc-id> <subnet-id> <vpc-cidr>
#
# The stack is left up for a look around. Delete it with
#   aws cloudformation delete-stack --stack-name recall-launch-test
# The bucket it created is retained on delete by design; remove it by hand.
set -euo pipefail
AMI=${1:?ami id}; VPC=${2:?vpc id}; SUBNET=${3:?subnet id}; CIDR=${4:?vpc cidr}
STACK=${STACK:-recall-launch-test}
REGION=${AWS_REGION:-us-east-1}
cd "$(dirname "$0")"

aws cloudformation deploy --region "$REGION" --stack-name "$STACK" \
  --template-file cloudformation/recall.yaml --capabilities CAPABILITY_IAM \
  --parameter-overrides ImageId="$AMI" VpcId="$VPC" SubnetIds="$SUBNET" \
  AllowedCidr="$CIDR" CreateLoadBalancer=no InstanceType=t4g.medium
out() { aws cloudformation describe-stacks --region "$REGION" --stack-name "$STACK" \
  --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text; }
ID=$(out InstanceId)
echo "instance $ID, endpoint $(out RecallEndpoint)"

echo "waiting for Session Manager"
for _ in $(seq 60); do
  [ "$(aws ssm describe-instance-information --region "$REGION" \
    --filters Key=InstanceIds,Values="$ID" --query 'length(InstanceInformationList)')" = 1 ] && break
  sleep 10
done

read -r -d '' CHECK <<'SH' || true
set -e
for i in $(seq 90); do curl -sf localhost:8080/healthz >/dev/null && break; sleep 2; done
recall --version; polign-server -version
systemctl is-active polign-server recall
AK=$(cat /etc/recall/admin-key)
K=$(curl -sf -H "Authorization: Bearer $AK" -d '{"namespace":"launchtest"}' localhost:8080/v1/keys | python3 -c 'import sys,json; print(json.load(sys.stdin)["key"])')
curl -sf -H "Authorization: Bearer $K" -d '{"text":"The customer prefers email over phone calls."}' localhost:8080/v1/remember >/dev/null
echo "recall: $(curl -sf -H "Authorization: Bearer $K" -d '{"question":"how should we reach out to them"}' localhost:8080/v1/recall)"
echo "no key: $(curl -s -o /dev/null -w '%{http_code}' -d '{}' localhost:8080/v1/recall)"
echo "db on localhost only: $(ss -ltn | awk '$4 ~ /:2300[01]$/ {print $4}' | tr '\n' ' ')"
SH
CMD=$(aws ssm send-command --region "$REGION" --instance-ids "$ID" \
  --document-name AWS-RunShellScript --parameters "$(python3 -c 'import json,sys; print(json.dumps({"commands":[sys.argv[1]]}))' "$CHECK")" \
  --query Command.CommandId --output text)
for _ in $(seq 60); do
  st=$(aws ssm get-command-invocation --region "$REGION" --command-id "$CMD" --instance-id "$ID" --query Status --output text 2>/dev/null || echo Pending)
  case "$st" in Pending|InProgress|Delayed) sleep 5 ;; *) break ;; esac
done
aws ssm get-command-invocation --region "$REGION" --command-id "$CMD" --instance-id "$ID" \
  --query '[Status,StandardOutputContent,StandardErrorContent]' --output text
