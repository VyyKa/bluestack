#!/bin/bash

# Create a directory for logs
mkdir -p /var/log/modsecurity

# Set permissions
chown -R www-data:www-data /var/log/modsecurity
chmod -R 775 /var/log/modsecurity
