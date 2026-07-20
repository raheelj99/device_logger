# Builds the supervisor into your C++ application. Drop these two files
# (devlogd_supervisor.{hpp,cpp}) into your source tree and link them, or use
# this standalone example to try it out:
#
#   cmake -S deploy/embed -B build/embed && cmake --build build/embed
#   DEVLOGD_BIN=$(pwd)/build/devlogd DEVLOGD_CFG=$(pwd)/config/devlogd.yaml \
#     ./build/embed/devlogd-supervisor-example
cmake_minimum_required(VERSION 3.16)
project(devlogd_supervisor CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

# Reusable library you link into your application.
add_library(devlogd_supervisor STATIC devlogd_supervisor.cpp)
target_include_directories(devlogd_supervisor PUBLIC ${CMAKE_CURRENT_SOURCE_DIR})
find_package(Threads REQUIRED)
target_link_libraries(devlogd_supervisor PUBLIC Threads::Threads)

# Standalone demo of the integration.
add_executable(devlogd-supervisor-example example_main.cpp)
target_link_libraries(devlogd-supervisor-example PRIVATE devlogd_supervisor)
