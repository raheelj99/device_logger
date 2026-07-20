# URL routing for the devlog_web project.
from django.urls import path

from devlog_web import views

urlpatterns = [
    path("jobs", views.jobs),
    path("entries", views.entries),
    path("verify/<str:device>", views.verify),
    path("report/<str:trace>", views.report),
    path("stats", views.stats),
]
