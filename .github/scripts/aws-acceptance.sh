#!/usr/bin/env bash
# The protected environment supplies only a disposable network's exact resources.
# The retained binary is necessary: mutation plans bind the original node recipe.
set -euo pipefail
umask 077
[[ "$PROOF_OPERATION" == doctor || "$PROOF_OPERATION" == upgrade || "$PROOF_OPERATION" == upgrade-interrupt ]]
[[ "$PROOF_BUCKET" =~ ^dashnet-upgrade-[a-z0-9-]+$ ]]
[[ "$PROOF_GROUP" =~ ^sg-[0-9a-f]+$ ]]
[[ "$PROOF_BINARY_SHA256" =~ ^[0-9a-f]{64}$ ]]
test -n "$PROOF_SSH_KEY"
work=$(mktemp -d "$RUNNER_TEMP/dashnet-proof.XXXXXXXX")
rule_id=''
cleanup() {
  code=$?
  trap - EXIT
  if [[ -n "$rule_id" ]]; then
    aws ec2 revoke-security-group-ingress --group-id "$PROOF_GROUP" \
      --security-group-rule-ids "$rule_id" > "$work/firewall-cleanup.json" || code=1
  fi
  for report in operator.log result.json interruption.json firewall-cleanup.json; do
    if [[ -f "$work/$report" ]]; then
      aws s3 cp --only-show-errors "$work/$report" \
        "s3://$PROOF_BUCKET/operations/$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT/$report" || code=1
    fi
  done
  rm -rf -- "$work"
  printf 'Isolated proof operation `%s` finished with exit `%s`. Detailed evidence is private.\n' \
    "$PROOF_OPERATION" "$code" >> "$GITHUB_STEP_SUMMARY"
  exit "$code"
}
trap cleanup EXIT
for file in dashnet known_hosts; do
  aws s3 cp --only-show-errors "s3://$PROOF_BUCKET/inputs/$file" "$work/$file"
done
printf '%s  %s\n' "$PROOF_BINARY_SHA256" "$work/dashnet" | sha256sum --check --status
chmod 700 "$work/dashnet"
printf '%s\n' "$PROOF_SSH_KEY" > "$work/key"
unset PROOF_SSH_KEY
artifact=deployment.json
if [[ "$PROOF_OPERATION" == upgrade* ]]; then
  [[ "$PROOF_PLAN_ID" =~ ^[0-9a-f]{64}$ ]]
  artifact=upgrade.json
fi
aws s3 cp --only-show-errors "s3://$PROOF_BUCKET/inputs/$artifact" "$work/plan.json"
args=(--plan "$work/plan.json" --ssh-key "$work/key" --known-hosts "$work/known_hosts"
      --observation-window 90s --timeout 100m --out "$work/result.json")
if [[ "$PROOF_OPERATION" == upgrade* ]]; then
  test "$(jq -r .id "$work/plan.json")" = "$PROOF_PLAN_ID"
  args+=(--confirm "$PROOF_PLAN_ID")
fi
# Only this runner's IPv4 is temporarily admitted; the exact rule ID is revoked.
egress=$(curl --fail --silent --show-error --max-time 20 https://checkip.amazonaws.com)
python3 -c 'import ipaddress,sys; assert ipaddress.ip_address(sys.argv[1]).version == 4' "$egress"
aws ec2 authorize-security-group-ingress --group-id "$PROOF_GROUP" --protocol tcp \
  --port 22 --cidr "$egress/32" > "$work/firewall.json"
rule_id=$(jq -r '.SecurityGroupRules[0].SecurityGroupRuleId // empty' "$work/firewall.json")
[[ "$rule_id" =~ ^sgr-[0-9a-f]+$ ]]
if [[ "$PROOF_OPERATION" == upgrade-interrupt ]]; then
  python3 .github/scripts/interrupt-proof.py "$work" "${args[@]}"
else
  "$work/dashnet" "$PROOF_OPERATION" "${args[@]}" > "$work/operator.log" 2>&1
fi
