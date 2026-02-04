#!/usr/bin/env python3
"""
System stats collector for tsstore via Unix socket.
Collects CPU, memory, disk, temperature, load, and network stats.
"""

import socket
import json
import time
import sys
import psutil
import os

# Configuration
SOCKET_PATH = "/home/tviviano/run/tsstore.sock"
STORE_NAME = "system-stats"
API_KEY = "tsstore_df14064e-e936-4312-b483-b1dc6b18645c"
SAMPLE_INTERVAL = 10  # seconds between samples

# Track network bytes for delta calculation
last_net_rx = 0
last_net_tx = 0
last_sample_time = 0

def get_cpu_temp():
    """Get CPU temperature on Raspberry Pi."""
    try:
        with open('/sys/class/thermal/thermal_zone0/temp', 'r') as f:
            return float(f.read().strip()) / 1000.0
    except:
        return 0.0

def connect_and_auth(socket_path, store_name, api_key):
    """Connect to Unix socket and authenticate."""
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.connect(socket_path)
    sock.settimeout(5.0)

    auth_cmd = f"AUTH {store_name} {api_key}\n"
    sock.send(auth_cmd.encode())

    response = sock.recv(1024).decode().strip()
    if not response.startswith("OK"):
        raise Exception(f"Authentication failed: {response}")

    return sock

def send_reading(sock, data):
    """Send a single reading to tsstore."""
    line = json.dumps(data) + "\n"
    sock.send(line.encode())

    response = sock.recv(1024).decode().strip()
    if not response.startswith("OK"):
        raise Exception(f"Write failed: {response}")

    parts = response.split()
    return int(parts[1]) if len(parts) >= 2 else None

def collect_stats():
    """Collect system statistics."""
    global last_net_rx, last_net_tx, last_sample_time

    # CPU (average over interval)
    cpu_pct = psutil.cpu_percent(interval=1)

    # Memory
    mem = psutil.virtual_memory()
    mem_pct = mem.percent
    mem_avail_mb = mem.available / (1024 * 1024)

    # Disk (root partition)
    disk = psutil.disk_usage('/')
    disk_pct = disk.percent
    disk_free_gb = disk.free / (1024 * 1024 * 1024)

    # Temperature
    cpu_temp = get_cpu_temp()

    # Load average (1 minute)
    load_1m = os.getloadavg()[0]

    # Network (cumulative bytes since boot, we'll track deltas)
    net = psutil.net_io_counters()
    current_time = time.time()

    # Calculate MB since last sample (or total if first sample)
    if last_sample_time > 0:
        net_rx_mb = (net.bytes_recv - last_net_rx) / (1024 * 1024)
        net_tx_mb = (net.bytes_sent - last_net_tx) / (1024 * 1024)
    else:
        net_rx_mb = 0
        net_tx_mb = 0

    last_net_rx = net.bytes_recv
    last_net_tx = net.bytes_sent
    last_sample_time = current_time

    return {
        "cpu_pct": round(cpu_pct, 1),
        "mem_pct": round(mem_pct, 1),
        "mem_avail_mb": round(mem_avail_mb, 1),
        "disk_pct": round(disk_pct, 1),
        "disk_free_gb": round(disk_free_gb, 2),
        "cpu_temp": round(cpu_temp, 1),
        "load_1m": round(load_1m, 2),
        "net_rx_mb": round(net_rx_mb, 3),
        "net_tx_mb": round(net_tx_mb, 3)
    }

def main():
    print(f"System stats collector for tsstore")
    print(f"Socket: {SOCKET_PATH}")
    print(f"Store: {STORE_NAME}")
    print(f"Sample interval: {SAMPLE_INTERVAL}s")
    print()

    sock = None
    reconnect_delay = 1
    sample_count = 0

    while True:
        try:
            if sock is None:
                print("Connecting to tsstore...")
                sock = connect_and_auth(SOCKET_PATH, STORE_NAME, API_KEY)
                print("Connected!")
                reconnect_delay = 1

            data = collect_stats()
            ts = send_reading(sock, data)
            sample_count += 1

            print(f"[{sample_count}] CPU:{data['cpu_pct']}% Mem:{data['mem_pct']}% "
                  f"Temp:{data['cpu_temp']}C Load:{data['load_1m']} "
                  f"Disk:{data['disk_pct']}%")

            time.sleep(SAMPLE_INTERVAL)

        except KeyboardInterrupt:
            print("\nShutting down...")
            if sock:
                try:
                    sock.send(b"QUIT\n")
                    sock.close()
                except:
                    pass
            sys.exit(0)

        except Exception as e:
            print(f"Error: {e}")
            if sock:
                try:
                    sock.close()
                except:
                    pass
                sock = None

            print(f"Reconnecting in {reconnect_delay}s...")
            time.sleep(reconnect_delay)
            reconnect_delay = min(reconnect_delay * 2, 60)

if __name__ == "__main__":
    main()
