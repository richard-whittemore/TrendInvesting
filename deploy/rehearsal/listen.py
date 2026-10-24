"""A stand-in for IB Gateway's API port in the first-day rehearsal: it only listens."""
import socket
import time

s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 4002))
s.listen()
while True:
    time.sleep(3600)
