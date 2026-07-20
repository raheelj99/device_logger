# Minimal Django settings — just enough to serve the JSON REST views. No DB,
# no auth, no templates: this project is a thin wrapper over devlog_client.
import os

BASE_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Dev-only key; override via env in any real deployment.
SECRET_KEY = os.environ.get("DJANGO_SECRET_KEY", "dev-only-not-secret")
DEBUG = os.environ.get("DJANGO_DEBUG", "1") == "1"
ALLOWED_HOSTS = ["*"]

INSTALLED_APPS = []
MIDDLEWARE = ["django.middleware.common.CommonMiddleware"]
ROOT_URLCONF = "devlog_web.urls"
WSGI_APPLICATION = "devlog_web.wsgi.application"
DATABASES = {}
USE_TZ = True
