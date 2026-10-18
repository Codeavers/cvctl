set -eu
export MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@localhost:9000"
cat > '/tmp/cvctl-bk-media-policy.json' <<'CVCTL_EOF'
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::cvctl/vps1/media/*"]},{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::cvctl"],"Condition":{"StringLike":{"s3:prefix":["vps1/media/*","vps1/media"]}}}]}
CVCTL_EOF
mc admin policy create local 'cvctl-bk-media' '/tmp/cvctl-bk-media-policy.json'
printf '%s\n' 's3cr3tvalue' | mc admin user add local 'bk-media'
case "$(mc admin user info local 'bk-media' --json)" in
  *'"policyName":"cvctl-bk-media"'* | *'"policyName":"cvctl-bk-media,'* | *',cvctl-bk-media"'* | *',cvctl-bk-media,'*) ;;
  *) mc admin policy attach local 'cvctl-bk-media' --user 'bk-media' ;;
esac
rm -f '/tmp/cvctl-bk-media-policy.json'
