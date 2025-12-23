#!/bin/bash
# Initialize PostgreSQL log directory with correct permissions and start PostgreSQL

# Create log directory if it doesn't exist
mkdir -p /var/log/postgresql

# Set ownership to postgres user
chown -R postgres:postgres /var/log/postgresql

# Set appropriate permissions
chmod -R 755 /var/log/postgresql

echo "PostgreSQL log directory initialized with correct permissions"

# Call the original PostgreSQL entrypoint with postgres command and config
exec docker-entrypoint.sh postgres -c config_file=/etc/postgresql/postgresql.conf
