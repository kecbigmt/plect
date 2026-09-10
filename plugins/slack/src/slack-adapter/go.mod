module github.com/kecbigmt/plecture/plugins/slack/src/slack-adapter

go 1.25.6

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/kecbigmt/plecture/contracts/channel-protocol v0.0.0
	github.com/kecbigmt/plecture/contracts/event v0.0.0
	github.com/slack-go/slack v0.22.0
)

require github.com/gorilla/websocket v1.5.3 // indirect

replace (
	github.com/kecbigmt/plecture/contracts/channel-protocol => ../../../../contracts/channel-protocol
	github.com/kecbigmt/plecture/contracts/event => ../../../../contracts/event
)
