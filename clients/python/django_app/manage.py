#!/usr/bin/env python3
# Django management entrypoint. Run: python manage.py runserver 0.0.0.0:8083
import os
import sys

if __name__ == "__main__":
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "devlog_web.settings")
    from django.core.management import execute_from_command_line

    execute_from_command_line(sys.argv)
