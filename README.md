# Dailymotion Operation Log

The Dailymotion OpLog is a Go agent meant to run on every PHP server, listening for UDP commands from the website describing every changes happening on a Dailymotion object.

The agent exposes an [Server Sent Event](http://dev.w3.org/html5/eventsource/) API for external consumers to be notified in real time about model changes.

For more information, see the [Wiki page](https://wiki.dailymotion.com/display/XP/OpLog) about this project.

## Install

Because this repository is private, you have to make sure git will use github using ssh if you are using 2FA. You can set this up with the following command:

    git config --global url."git@github.com:".insteadOf "https://github.com/"

Then to install the project, execute the following two commands:

    go get github.com/dailymotion/oplog
    go build -o /usr/local/bin/oplogd github.com/dailymotion/oplog/cmd/oplogd

## UDP API

To send operation events to the agent, an UDP datagram a JSON object must be crafted and sent on the agent's UDP port (8042 by default).

The format of the message is as follow:

```javascript
{
    "event": "insert",
    "parents": ["user/x1234"],
    "type": "video",
    "id": "x345",
}
```

All keys are required:

* `event`: The type of event. Can be `INSERT`, `UPDATE` or `DELETE`.
* `parents`: The list of parent objects of the modified object formated as `type/xid`.
* `type`: The object type (i.e.: `video`, `user`, `playlist`…)
* `id`: The object xid of the impacted object.

Only xid must be used, numerical ids aren't accepted.

## SSE API

The SSE API runs on the same port as UDP API but using TCP. The W3C SSE protocol is respected by the book. To connect to the API, a GET on `/` with the `Accept: text/event-stream` header must be performed.

On each received event, the client must store the last event id and submit it back to the server on reconnect using the `Last-Event-ID` HTTP header. The client must then ensure the `Last-Event-ID` header is sent back in the response. It may happen that the id defined by `Last-Event-ID` is no longer available, in this case, the agent won't send the backlog and will ignore the `Last-Event-ID` header. You may want to perform a full sync when such case happen.

The following filters can be passed as query-string:
* `types` A list of object types to filter on separated by comas (i.e.: `types=video,user`).
* `parents` A coma separated list of `type/xid` to filter on

```
GET / HTTP/1.1
Accept: text/event-stream

HTTP/1.1 200 OK
Content-Type: text/event-stream; charset=utf-8

id: 545b55c7f095528dd0f3863c
event: insert
data: {"timestamp":"2014-11-06T03:04:39.041-08:00","parents":["x1234"],"type":"video","id":"x345"}

id: 545b55c8f095528dd0f3863d
event: delete
data: {"timestamp":"2014-11-06T03:04:40.091-08:00","parents":["x1234"],"type":"video","id":"x345"}

…
```

### Full Sync

If required, a full sync can be performed before streaming live updates. To perform a full sync, pass `0` as `Last-Event-ID`. Numeric event ids with lesser than 24 digits are considered as a sync id, which represent a milliseconds UNIX timestamp. By passing a millisecond timestamp, you are asking for syncing any objects that have been modified after this date. Passing `0` thus ensures every objects will be synced.

If a full sync is interrupted during the transfer, the same mechanism as for live events will be used. Once sync is done, the stream will automatically switch to live events stream so your component is ensured not to miss any updates.

## Status Endpoint

The agent exposes a `/status` endpoint over HTTP to show some statistics about the agent. A JSON object is returned with the following fields:

* `events_received`: Total number of events received on the UDP interface
* `events_ingested`: Total number of events ingested into MongoDB with success
* `events_error`: Total number of events received on the UDP interface with an invalid format
* `events_discarded`: Total number of events discarded because the queue was full
* `queue_size`: Current number of events in the ingestion queue
* `queue_max_size`:  Maximum number of events allowed in the ingestion queue before discarding events
* `clients`: Number of clients connected to the SSE API

```javascript
GET /status

HTTP/1.1 200 OK
Content-Length: 144
Content-Type: application/json
Date: Thu, 06 Nov 2014 10:40:25 GMT

{
    "clients": 0,
    "events_discarded": 0,
    "events_error": 0,
    "events_ingested": 0,
    "events_received": 0,
    "queue_max_size": 100000,
    "queue_size": 0,
    "status": "OK"
}
```


