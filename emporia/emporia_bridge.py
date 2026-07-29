#!/usr/bin/env python3
"""HomeForge Emporia bridge — polls the Emporia cloud (pyemvue, AWS Cognito auth) and
publishes per-channel power to HomeForge's MQTT broker as emporia/<key>/power (watts).
Self-contained sidecar; keys off the channel names set in the Emporia app, so it
auto-adapts when CTs are reallocated/renamed. Mains ("1,2,3") -> emporia/mains."""
import os, time, re, datetime
import paho.mqtt.client as mqtt
import pyemvue
from pyemvue.enums import Scale, Unit

EMAIL = os.environ["EMPORIA_EMAIL"]
PASSWORD = os.environ["EMPORIA_PASSWORD"]
MQTT_HOST = os.environ.get("MQTT_HOST", "127.0.0.1")
MQTT_PORT = int(os.environ.get("MQTT_PORT", "1885"))
MQTT_USER = os.environ.get("MQTT_USER", "homeforge")
MQTT_PASS = os.environ.get("MQTT_PASS", "homeforge")
POLL = int(os.environ.get("POLL_SECONDS", "30"))


def slug(s):
    return re.sub(r"_+", "_", re.sub(r"[^a-z0-9]+", "_", (s or "").lower())).strip("_")


def keyfor(chnum, name):
    if chnum == "1,2,3":
        return "mains"
    if str(chnum).lower() == "balance":
        return "balance"
    return slug(name) or ("channel_" + slug(str(chnum)))


def main():
    vue = pyemvue.PyEmVue()
    vue.login(username=EMAIL, password=PASSWORD)
    print("emporia: logged in", flush=True)

    devs = vue.get_devices()
    gids, name_map = [], {}
    for d in devs:
        if d.device_gid not in gids:
            gids.append(d.device_gid)
        for ch in d.channels:
            name_map[(d.device_gid, ch.channel_num)] = ch.name

    cli = mqtt.Client()
    cli.username_pw_set(MQTT_USER, MQTT_PASS)
    cli.connect(MQTT_HOST, MQTT_PORT, 60)
    cli.loop_start()

    while True:
        try:
            usage = vue.get_device_list_usage(
                deviceGids=gids,
                instant=datetime.datetime.now(datetime.timezone.utc),
                scale=Scale.MINUTE.value, unit=Unit.KWH.value)
            n = 0
            for gid, dev in usage.items():
                for chnum, ch in dev.channels.items():
                    kwh = ch.usage or 0.0
                    watts = round(kwh * 60000.0)  # kWh over 1 min -> average watts
                    name = name_map.get((gid, chnum)) or ch.name or ""
                    cli.publish(f"emporia/{keyfor(chnum, name)}/power", str(watts), qos=0, retain=True)
                    n += 1
            print(f"emporia: published {n} channels", flush=True)
        except Exception as e:
            print("emporia: error", repr(e), flush=True)
            try:
                vue.login(username=EMAIL, password=PASSWORD)  # re-auth and retry next cycle
            except Exception as e2:
                print("emporia: relogin failed", repr(e2), flush=True)
        time.sleep(POLL)


if __name__ == "__main__":
    main()
