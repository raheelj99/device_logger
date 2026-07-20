# FastAPI REST wrapper over the shared devlog_client service layer.
# Run: uvicorn main:app --app-dir clients/python/fastapi_app --port 8082
from typing import Optional

from fastapi import FastAPI
from fastapi.responses import JSONResponse
from pydantic import BaseModel

from devlog_client import service

app = FastAPI(title="devlog-client (FastAPI)")


class JobRequest(BaseModel):
    trace_id: Optional[str] = ""
    device_id: Optional[str] = ""


@app.post("/jobs")
def post_jobs(body: JobRequest = JobRequest()):
    return service.publish_job(body.trace_id or "", body.device_id or "")


@app.get("/entries")
def get_entries(trace: str = "", since_ms: int = 15 * 60 * 1000):
    return service.query_entries(trace_id=trace, since_ms=since_ms)


@app.get("/verify/{device}")
def get_verify(device: str):
    return service.verify_range(device_id=device)


@app.get("/report/{trace}")
def get_report(trace: str):
    return service.export_report(trace)


@app.get("/stats")
def get_stats():
    return service.get_stats()


@app.exception_handler(Exception)
def on_error(_request, exc):
    return JSONResponse(status_code=500, content={"error": str(exc)})
