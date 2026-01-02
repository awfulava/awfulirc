awfulirc

# Username and password authentication

The fastest way to get started is to run the server with username and
password:

```sh
awfulirc --username "Your username" --password "Your password"
```

Then, connect to the bridge with your favorite IRC client:

```
/CONNECT 127.0.0.1 6667
```

# Token authentication

If you are paranoid about your credentials leaking, you can use token
authentication as well. First generate a token with
`gen-token.sh`. Instead of passing in username and password to
awfulirc, pass the token file:

```sh
awfulirc --authfile ~/.sa-login
```

Then, connect to the bridge with your favorite IRC client:

```
/CONNECT 127.0.0.1 6667
```
