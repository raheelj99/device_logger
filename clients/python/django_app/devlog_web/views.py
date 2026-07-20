# Django REST views — thin delegates to the shared devlog_client service layer.
import json

from django.http import JsonResponse
from django.views.decorators.csrf import csrf_exempt

from devlog_client import service


def _guard(fn):
    """Return a 500 JSON envelope on any service error (mirrors the other apps)."""
    def wrapped(request, *args, **kwargs):
        try:
            return fn(request, *args, **kwargs)
        except Exception as err:  # noqa: BLE001
            return JsonResponse({"error": str(err)}, status=500)
    wrapped.__name__ = fn.__name__
    return wrapped


@csrf_exempt
@_guard
def jobs(request):
    body = json.loads(request.body or b"{}")
    return JsonResponse(service.publish_job(body.get("trace_id", ""), body.get("device_id", "")))


@_guard
def entries(request):
    trace = request.GET.get("trace", "")
    since_ms = int(request.GET.get("since_ms", 15 * 60 * 1000))
    return JsonResponse(service.query_entries(trace_id=trace, since_ms=since_ms), safe=False)


@_guard
def verify(request, device):
    return JsonResponse(service.verify_range(device_id=device))


@_guard
def report(request, trace):
    return JsonResponse(service.export_report(trace))


@_guard
def stats(request):
    return JsonResponse(service.get_stats())
