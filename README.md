# vp9-streamer (WebRTC)

Build:
```
$ go build -o vp9-streamer *.go
```

Options:
```
$ ./vp9-streamer -h
Usage: ./vp9-streamer [OPTION]... [INTERFACE]...

Options
 -h, --help            display this help and exit
 -6                    add support for ipv6 ice candidates
 -A, --http-addr=IP    httpd listen address, default 127.0.0.1
 -P, --http-port=PORT  httpd listen port, default 8080
 -a, --rtp-addr=IP     rtp listen address, default 0.0.0.0, ::
 -p, --rtp-port=PORT   rtp listen port, default 8514

Interfaces
 list of allowed interfaces for rtp stream, default: allow all

```

Example usage:
```
$ ffmpeg -hide_banner -nostats \
  -vaapi_device /dev/dri/renderD128 -f v4l2 -standard PAL -i /dev/video0 \
  -vf 'format=nv12,hwupload,vaapi_scale=format=nv12' -c:v vp9_vaapi -g 15 -f ivf - | \
  vp9-streamer -6 --http-addr 127.0.0.1 --http-port 8080 --rtp-port 8514 eth0 eth1
```
