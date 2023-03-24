# webrtc-zero-downtime-restart

webrtc-zero-downtime-restart is a simple Pion WebRTC broadcast server that can be restarted
without disconnecting users. All the WebRTC state is suspended to disk. The
next time the process is started that information is read into memory. All
the remote PeerConnections will be unaware that they are even connected to a new
process. This gives us the following benefits.

### Painless Deploys
No more migrating users to a new process when you want to deploy code. At anytime you
can replace the binary you are running and no users will be impacted. You don't need
to implement extra signaling/error handling for your deploy process.

### Easier Scaling
Move clients to an entirely different host without them knowing. You can do this with
zero interruption in service or additional signaling.

### Greater Resiliency
Since server state is constantly being written to disk you don't need to worry about
crashes anymore. If you server goes down (and then restarts) it will automatically resume
the last known good state.

## Running

Execute `go run github.com/Sean-Der/webrtc-zero-downtime-restart@latest`. This will start the server and print

```
Open http://localhost:8080 to access this demo
```

You can then access it at [http://localhost:8080](http://localhost:8080). The first user to connect will broadcast
their webcam. Every user after can watch the broadcasted video.

At anytime you can start+stop the process in your terminal. Users will not be disconnected and will
be able to continue talking when the process is started again.

## What is next

This demo uses reflection to access internal Pion WebRTC APIs. We will be working on designing the final
APIs for the next major release of Pion WebRTC. We would love your feedback ideas either on the
[tracking issue]() or [Slack](https://pion.ly/slack)
