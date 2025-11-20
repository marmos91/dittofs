#!/bin/bash

# This script runs when Localstack is ready
# It sets up any required resources for integration tests

echo "Setting up Localstack for DittoFS integration tests..."

# Create test buckets (optional - tests can create their own)
# awslocal s3 mb s3://dittofs-test-bucket

echo "Localstack setup complete!"
