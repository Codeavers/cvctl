set -eu
export MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@localhost:9000"
mc mb --ignore-existing local/prescriptions
mc mb --ignore-existing local/products
mc anonymous set none local/prescriptions
cat > /tmp/cvctl-anon-products.json <<'CVCTL_EOF'
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::products/*"]}]}
CVCTL_EOF
mc anonymous set-json /tmp/cvctl-anon-products.json local/products
cat > /tmp/cvctl-app-policy.json <<'CVCTL_EOF'
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::prescriptions/*","arn:aws:s3:::products/*"]},{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::prescriptions","arn:aws:s3:::products"]}]}
CVCTL_EOF
mc admin policy create local cvctl-app /tmp/cvctl-app-policy.json
printf '%s\n' 'appsecret1234567' | mc admin user add local 'media-app'
case "$(mc admin user info local 'media-app' --json)" in
  *'"policyName":"cvctl-app"'* | *'"policyName":"cvctl-app,'* | *',cvctl-app"'* | *',cvctl-app,'*) ;;
  *) mc admin policy attach local cvctl-app --user 'media-app' ;;
esac
rm -f /tmp/cvctl-app-policy.json /tmp/cvctl-anon-*.json
echo @@buckets
mc ls --json local
