import json
import os

from fastapi import FastAPI

app = FastAPI()

ORDERS_TOPIC = os.getenv("ORDERS_TOPIC")


# get_user_stats returns aggregated spending for a user.
@app.get("/api/analytics/users/{user_id}")
def get_user_stats(user_id: str):
    rows = db.execute(
        "SELECT user_id, amount FROM analytics_events WHERE user_id = ?",
        user_id,
    )
    return {"user_id": user_id, "events": rows}


# start_consumer subscribes to order events and stores them.
def start_consumer():
    consumer.subscribe([ORDERS_TOPIC])
    for message in consumer:
        event = json.loads(message.value)
        store_event(event["user_id"], event["amount"])


# store_event persists one order event for analytics.
def store_event(user_id, amount):
    db.execute(
        "INSERT INTO analytics_events (user_id, amount) VALUES (?, ?)",
        user_id,
        amount,
    )
