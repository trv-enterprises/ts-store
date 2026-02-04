#!/usr/bin/env python3
"""
SenseHat data collector for tsstore via Unix socket.
Reads temperature, humidity, pressure, and accelerometer data.
"""

import socket
import json
import time
import sys
from sense_hat import SenseHat

# Configuration
SOCKET_PATH = "/home/tviviano/run/tsstore.sock"
STORE_NAME = "sensehat"
API_KEY = "tsstore_8bd1bfd5-9e42-426c-859e-b01442ffb338"
SAMPLE_RATE = 1.0  # Hz (samples per second)

def connect_and_auth(socket_path, store_name, api_key):
    """Connect to Unix socket and authenticate."""
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.connect(socket_path)
    sock.settimeout(5.0)

    # Authenticate
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

    # Parse timestamp from response: "OK <timestamp>"
    parts = response.split()
    if len(parts) >= 2:
        return int(parts[1])
    return None

def main():
    print(f"SenseHat to tsstore collector")
    print(f"Socket: {SOCKET_PATH}")
    print(f"Store: {STORE_NAME}")
    print(f"Sample rate: {SAMPLE_RATE} Hz")
    print()

    # Initialize SenseHat
    sense = SenseHat()
    sense.clear()

    # Show startup indicator
    sense.show_letter("S", text_colour=[0, 255, 0])
    time.sleep(0.5)
    sense.clear()

    sock = None
    reconnect_delay = 1
    sample_count = 0

    while True:
        try:
            # Connect if needed
            if sock is None:
                print("Connecting to tsstore...")
                sock = connect_and_auth(SOCKET_PATH, STORE_NAME, API_KEY)
                print("Connected!")
                reconnect_delay = 1

            # Read sensors
            accel = sense.get_accelerometer_raw()
            data = {
                "temp": round(sense.get_temperature(), 2),
                "humidity": round(sense.get_humidity(), 2),
                "pressure": round(sense.get_pressure(), 2),
                "accel_x": round(accel['x'], 4),
                "accel_y": round(accel['y'], 4),
                "accel_z": round(accel['z'], 4)
            }

            # Send to tsstore
            ts = send_reading(sock, data)
            sample_count += 1

            if sample_count % 60 == 0:  # Log every minute at 1Hz
                print(f"Samples: {sample_count}, Last: {data}")

            # Wait for next sample
            time.sleep(1.0 / SAMPLE_RATE)

        except KeyboardInterrupt:
            print("\nShutting down...")
            if sock:
                try:
                    sock.send(b"QUIT\n")
                    sock.close()
                except:
                    pass
            sense.clear()
            sys.exit(0)

        except Exception as e:
            print(f"Error: {e}")
            if sock:
                try:
                    sock.close()
                except:
                    pass
                sock = None

            # Exponential backoff
            print(f"Reconnecting in {reconnect_delay}s...")
            time.sleep(reconnect_delay)
            reconnect_delay = min(reconnect_delay * 2, 60)

if __name__ == "__main__":
    main()
