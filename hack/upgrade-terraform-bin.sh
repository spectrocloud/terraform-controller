#!/bin/bash

# Get the current terraform version
current_version=$(bin/terraform -version | head -n 1 | cut -d 'v' -f 2)

# Get the latest terraform version from the official website
latest_version=$(curl -s https://releases.hashicorp.com/terraform/ | grep -oP '(?<=href="/terraform/)[0-9.]+(?=/")' | head -n 1)

# Compare the versions and update if needed
if [ "$current_version" != "$latest_version" ]; then
  echo "Your terraform version is $current_version, but the latest version is $latest_version."
  echo "Updating terraform to the latest version..."
  # Download the latest terraform binary for Linux
  wget https://releases.hashicorp.com/terraform/$latest_version/terraform_${latest_version}_linux_amd64.zip
  # Unzip the binary and move it to /usr/local/bin
  unzip terraform_${latest_version}_linux_amd64.zip
  rm bin/terraform
  mv terraform bin/
  # Remove the zip file
  rm terraform_${latest_version}_linux_amd64.zip
  echo "Terraform updated successfully."
else
  echo "Your terraform version is up to date."
fi
