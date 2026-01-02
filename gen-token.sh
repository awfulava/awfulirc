#!/usr/bin/env bash

echo -n "Username: "
read username
echo -n "Password (will not print): "
read -s password

if [[ -z "$filename" ]]; then
	filename=~/.sa-login
fi
echo "Generating auth file $filename"

curl \
	--cookie-jar $filename \
	-F action=login \
	-F username="$username" \
	-F password="$password" \
	-F next="/index.php?json=1" \
	"https://forums.somethingawful.com/account.php?json=1"

echo "Run the following to start the bridge:"
echo ""
echo "    awfulirc --authfile $filename"
echo ""
