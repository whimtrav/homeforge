#!/usr/bin/env python3
"""Hubspace (Afero) -> HomeForge MQTT bridge.

Uses aioafero (the same lib HA's hubspace integration uses) to talk to the
Afero cloud with username+password (self-refreshing). Publishes each light /
outlet / fan to hubspace/<dev>/<key> (retained) and accepts commands on
hubspace/<dev>/set {power|brightness|speed: value}.

HomeForge's handleHubspace (hubspace/+/+) turns those into switch.hubspace_* +
number.hubspace_*_{brightness,speed}; handleServiceCallMQTT routes control back
here via hubspace/<dev>/set.
"""
import asyncio
import json
import logging
import os

import aiohttp
import paho.mqtt.client as mqtt
from aioafero.v1 import AferoBridgeV1

logging.basicConfig(level=logging.INFO, format="%(asctime)s hubspace: %(message)s")
log = logging.getLogger("hubspace")

MQTT_HOST = os.environ.get("MQTT_HOST", "127.0.0.1")
MQTT_PORT = int(os.environ.get("MQTT_PORT", "1885"))
MQTT_USER = os.environ.get("MQTT_USER", "homeforge")
MQTT_PASS = os.environ.get("MQTT_PASS", "homeforge")
POLL = int(os.environ.get("POLL_SECONDS", "30"))

HS_USER = os.environ.get("HS_USERNAME", "").strip()
HS_PASS = os.environ.get("HS_PASSWORD", "").strip()
HS_TOKEN = os.environ.get("HS_TOKEN", "").strip() or None
HS_CLIENT = os.environ.get("HS_CLIENT", "hubspace").strip()


def slug(s: str) -> str:
    out, prev = [], False
    for ch in str(s).lower():
        if ch.isalnum():
            out.append(ch)
            prev = False
        elif not prev:
            out.append("_")
            prev = True
    return "".join(out).strip("_") or "device"


def dev_name(resource) -> str:
    di = getattr(resource, "device_information", None)
    name = getattr(di, "name", None) if di else None
    return name or getattr(resource, "id", "device")


class Bridge:
    def __init__(self, m: mqtt.Client, api: AferoBridgeV1):
        self.m = m
        self.api = api
        # slug -> {"kind","rid","instance"}
        self.cmd = {}

    def pub(self, dev, key, val):
        if val is None:
            return
        self.m.publish(f"hubspace/{dev}/{key}", str(val), retain=True)

    def publish_all(self):
        cmd = {}
        n = 0
        # --- switches (outlets, possibly multi-instance) ---
        for r in self.api.switches:
            base = slug(dev_name(r))
            try:
                on_map = r.on  # dict instance -> feature
            except Exception:
                continue
            instances = list(on_map.keys()) if hasattr(on_map, "keys") else [None]
            for inst in instances:
                # Skip the device-level aggregate (None) when named instances exist.
                if inst is None and len(instances) > 1:
                    continue
                s = base if (inst is None) else f"{base}_{slug(inst)}"
                feat = on_map.get(inst) if hasattr(on_map, "get") else None
                on = getattr(feat, "on", None)
                self.pub(s, "power", "on" if on else "off")
                cmd[s] = {"kind": "switch", "rid": r.id, "instance": inst}
                n += 1
        # --- lights (some Hubspace "lights" are dimmable plugs) ---
        for r in self.api.lights:
            s = slug(dev_name(r))
            self.pub(s, "power", "on" if getattr(r, "is_on", False) else "off")
            if getattr(r, "supports_dimming", False) or getattr(r, "dimming", None):
                b = getattr(r, "brightness", None)
                if b is not None:
                    self.pub(s, "brightness", int(b))
            cmd[s] = {"kind": "light", "rid": r.id, "instance": None}
            n += 1
        # --- fans ---
        for r in self.api.fans:
            s = slug(dev_name(r))
            self.pub(s, "power", "on" if getattr(r, "is_on", False) else "off")
            if getattr(r, "supports_speed", False):
                sp = getattr(getattr(r, "speed", None), "speed", None)
                if sp is not None:
                    self.pub(s, "speed", int(sp))
            cmd[s] = {"kind": "fan", "rid": r.id, "instance": None}
            n += 1
        self.cmd = cmd
        return n

    async def apply(self, dev, payload):
        target = self.cmd.get(dev)
        if not target:
            log.warning("set for unknown device %s", dev)
            return
        kind, rid, inst = target["kind"], target["rid"], target["instance"]
        try:
            if kind == "switch":
                ctrl = self.api.switches
            elif kind == "light":
                ctrl = self.api.lights
            else:
                ctrl = self.api.fans
            if "power" in payload:
                on = str(payload["power"]).lower() in ("on", "true", "1")
                if kind == "switch":
                    await ctrl.set_state(device_id=rid, on=on, instance=inst)
                else:
                    await ctrl.set_state(device_id=rid, on=on)
            elif "brightness" in payload:
                await ctrl.set_state(device_id=rid, brightness=int(float(payload["brightness"])))
            elif "speed" in payload:
                v = int(float(payload["speed"]))
                if v <= 0:
                    await ctrl.set_state(device_id=rid, on=False)
                else:
                    await ctrl.set_state(device_id=rid, on=True, speed=v)
            log.info("set %s %s", dev, payload)
        except Exception as e:  # noqa: BLE001
            log.warning("set %s failed: %s: %s", dev, type(e).__name__, e)


async def main():
    if not (HS_USER and HS_PASS):
        log.error("HS_USERNAME + HS_PASSWORD required")
        return

    m = mqtt.Client()
    m.username_pw_set(MQTT_USER, MQTT_PASS)
    m.connect(MQTT_HOST, MQTT_PORT, 60)
    m.loop_start()
    log.info("MQTT -> %s:%s", MQTT_HOST, MQTT_PORT)

    loop = asyncio.get_running_loop()

    async with aiohttp.ClientSession() as session:
        api = AferoBridgeV1(
            HS_USER, HS_PASS, refresh_token=HS_TOKEN, session=session,
            polling_interval=POLL, afero_client=HS_CLIENT,
        )
        await api.initialize()
        await api.async_block_until_done()
        log.info("connected; lights=%d switches=%d fans=%d",
                 len(list(api.lights)), len(list(api.switches)), len(list(api.fans)))

        bridge = Bridge(m, api)

        def on_message(_c, _u, msg):
            parts = msg.topic.split("/")
            if len(parts) != 3 or parts[2] != "set":
                return
            try:
                payload = json.loads(msg.payload.decode())
            except Exception:
                return
            asyncio.run_coroutine_threadsafe(bridge.apply(parts[1], payload), loop)

        m.on_message = on_message
        m.subscribe("hubspace/+/set")

        while True:
            try:
                count = bridge.publish_all()
                log.info("published %d devices", count)
            except Exception as e:  # noqa: BLE001
                log.warning("publish error: %s: %s", type(e).__name__, e)
            await asyncio.sleep(POLL)


if __name__ == "__main__":
    asyncio.run(main())
