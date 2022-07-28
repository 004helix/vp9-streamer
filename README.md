# vp9-streamer (WebRTC)

Example:
```
$ ffmpeg -hide_banner -nostats \
  -vaapi_device /dev/dri/renderD128 -f v4l2 -standard PAL -i /dev/video0 \
  -vf 'format=nv12,hwupload,vaapi_scale=format=nv12' -c:v vp9_vaapi -g 5 -f ivf - | \
  vp9-streamer -6 --http-addr 127.0.0.1 --http-port 8080 --rtp-port 8514 eth0 eth1
```
