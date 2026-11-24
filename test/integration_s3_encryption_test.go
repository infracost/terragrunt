package test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gruntwork-io/terragrunt/aws_helper"
	"github.com/gruntwork-io/terragrunt/config"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/remote"
)

const (
	s3SSEAESFixturePath            = "fixture-s3-encryption/sse-aes"
	s3SSECustomKeyFixturePath      = "fixture-s3-encryption/custom-key"
	s3SSBasicEncryptionFixturePath = "fixture-s3-encryption/basic-encryption"
)

func TestTerragruntS3SSEAES(t *testing.T) {
	t.Parallel()

	cleanupTerraformFolder(t, s3SSEAESFixturePath)

	s3BucketName := fmt.Sprintf("terragrunt-test-bucket-%s", strings.ToLower(uniqueId()))
	lockTableName := fmt.Sprintf("terragrunt-test-locks-%s", strings.ToLower(uniqueId()))

	defer deleteS3Bucket(t, TERRAFORM_REMOTE_STATE_S3_REGION, s3BucketName)
	defer cleanupTableForTest(t, lockTableName, TERRAFORM_REMOTE_STATE_S3_REGION)

	tmpTerragruntConfigPath := createTmpTerragruntConfig(t, s3SSEAESFixturePath, s3BucketName, lockTableName, config.DefaultTerragruntConfigPath)

	runTerragrunt(t, fmt.Sprintf("terragrunt apply -auto-approve --terragrunt-non-interactive --terragrunt-config %s --terragrunt-working-dir %s", tmpTerragruntConfigPath, s3SSEAESFixturePath))

	sseRule := bucketSSERule(t, s3BucketName)
	assert.Equal(t, s3types.ServerSideEncryptionAes256, sseRule.SSEAlgorithm)
	assert.Nil(t, sseRule.KMSMasterKeyID)
}

func TestTerragruntS3SSECustomKey(t *testing.T) {
	t.Parallel()

	cleanupTerraformFolder(t, s3SSECustomKeyFixturePath)

	s3BucketName := fmt.Sprintf("terragrunt-test-bucket-%s", strings.ToLower(uniqueId()))
	lockTableName := fmt.Sprintf("terragrunt-test-locks-%s", strings.ToLower(uniqueId()))

	defer deleteS3Bucket(t, TERRAFORM_REMOTE_STATE_S3_REGION, s3BucketName)
	defer cleanupTableForTest(t, lockTableName, TERRAFORM_REMOTE_STATE_S3_REGION)

	tmpTerragruntConfigPath := createTmpTerragruntConfig(t, s3SSECustomKeyFixturePath, s3BucketName, lockTableName, config.DefaultTerragruntConfigPath)

	runTerragrunt(t, fmt.Sprintf("terragrunt apply -auto-approve --terragrunt-non-interactive --terragrunt-config %s --terragrunt-working-dir %s", tmpTerragruntConfigPath, s3SSECustomKeyFixturePath))

	sseRule := bucketSSERule(t, s3BucketName)
	assert.Equal(t, s3types.ServerSideEncryptionAwsKms, sseRule.SSEAlgorithm)
	assert.True(t, strings.HasSuffix(aws.ToString(sseRule.KMSMasterKeyID), "alias/dedicated-test-key"))

}

func TestTerragruntS3SSEKeyNotReverted(t *testing.T) {
	t.Parallel()

	cleanupTerraformFolder(t, s3SSBasicEncryptionFixturePath)

	s3BucketName := fmt.Sprintf("terragrunt-test-bucket-%s", strings.ToLower(uniqueId()))
	lockTableName := fmt.Sprintf("terragrunt-test-locks-%s", strings.ToLower(uniqueId()))

	defer deleteS3Bucket(t, TERRAFORM_REMOTE_STATE_S3_REGION, s3BucketName)
	defer cleanupTableForTest(t, lockTableName, TERRAFORM_REMOTE_STATE_S3_REGION)

	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}

	tmpTerragruntConfigPath := createTmpTerragruntConfig(t, s3SSBasicEncryptionFixturePath, s3BucketName, lockTableName, config.DefaultTerragruntConfigPath)
	runTerragruntRedirectOutput(t, fmt.Sprintf("terragrunt apply -auto-approve --terragrunt-non-interactive --terragrunt-working-dir %s", filepath.Dir(tmpTerragruntConfigPath)), &stdout, &stderr)
	output := stderr.String()

	// verify that bucket encryption message is not printed
	assert.NotContains(t, output, "Bucket Server-Side Encryption")

	stdout = bytes.Buffer{}
	stderr = bytes.Buffer{}
	tmpTerragruntConfigPath = createTmpTerragruntConfig(t, s3SSBasicEncryptionFixturePath, s3BucketName, lockTableName, config.DefaultTerragruntConfigPath)
	runTerragruntRedirectOutput(t, fmt.Sprintf("terragrunt apply -auto-approve --terragrunt-non-interactive --terragrunt-working-dir %s", filepath.Dir(tmpTerragruntConfigPath)), &stdout, &stderr)
	output = stderr.String()
	assert.NotContains(t, output, "Bucket Server-Side Encryption")

	// verify that encryption key is not reverted
	sseRule := bucketSSERule(t, s3BucketName)
	assert.Equal(t, s3types.ServerSideEncryptionAwsKms, sseRule.SSEAlgorithm)
	assert.True(t, strings.HasSuffix(aws.ToString(sseRule.KMSMasterKeyID), "alias/dedicated-test-key"))
}

// bucketSSERule returns the bucket's single default server-side encryption rule.
func bucketSSERule(t *testing.T, bucketName string) *s3types.ServerSideEncryptionByDefault {
	t.Helper()

	mockOptions, err := options.NewTerragruntOptionsForTest("integration_test")
	require.NoError(t, err)

	client, err := remote.CreateS3Client(&aws_helper.AwsSessionConfig{Region: TERRAFORM_REMOTE_STATE_S3_REGION}, mockOptions)
	require.NoError(t, err)

	resp, err := client.GetBucketEncryption(context.Background(), &s3.GetBucketEncryptionInput{Bucket: aws.String(bucketName)})
	require.NoError(t, err)
	require.Len(t, resp.ServerSideEncryptionConfiguration.Rules, 1)

	sseRule := resp.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault
	require.NotNil(t, sseRule)
	return sseRule
}
