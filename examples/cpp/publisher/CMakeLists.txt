cmake_minimum_required(VERSION 3.20)
project(devlog_publisher CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

# Dependencies (e.g. via vcpkg: vcpkg install paho-mqttpp3 protobuf)
find_package(PahoMqttCpp CONFIG REQUIRED)
find_package(Protobuf CONFIG REQUIRED)

# Generate C++ from the shared schema — the same .proto the Go service uses.
set(PROTO_ROOT ${CMAKE_CURRENT_SOURCE_DIR}/../../../api/proto)
add_library(devicelog_proto OBJECT ${PROTO_ROOT}/devicelog/v1/log.proto)
target_link_libraries(devicelog_proto PUBLIC protobuf::libprotobuf)
target_include_directories(devicelog_proto PUBLIC ${CMAKE_CURRENT_BINARY_DIR})
protobuf_generate(TARGET devicelog_proto IMPORT_DIRS ${PROTO_ROOT})

add_executable(publisher main.cpp)
target_link_libraries(publisher PRIVATE devicelog_proto PahoMqttCpp::paho-mqttpp3)
