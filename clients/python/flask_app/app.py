# Flask REST wrapper over the shared devlog_client service layer.
# Run: python clients/python/flask_app/app.py   (or `flask --app app run`)
from flask import Flask, jsonify, request

from devlog_client import service

app = Flask(__name__)


@app.post("/jobs")
def post_jobs():
    body = request.get_json(silent=True) or {}
    return jsonify(service.publish_job(body.get("trace_id", ""), body.get("device_id", "")))


@app.get("/entries")
def get_entries():
    trace = request.args.get("trace", "")
    since_ms = int(request.args.get("since_ms", 15 * 60 * 1000))
    return jsonify(service.query_entries(trace_id=trace, since_ms=since_ms))


@app.get("/verify/<device>")
def get_verify(device):
    return jsonify(service.verify_range(device_id=device))


@app.get("/report/<trace>")
def get_report(trace):
    return jsonify(service.export_report(trace))


@app.get("/stats")
def get_stats():
    return jsonify(service.get_stats())


@app.errorhandler(Exception)
def on_error(err):
    return jsonify({"error": str(err)}), 500


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8081)
