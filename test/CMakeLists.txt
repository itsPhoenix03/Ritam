cmake_minimum_required(VERSION 3.14)
project(trade_engine LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
  set_property(CACHE CMAKE_BUILD_TYPE PROPERTY STRINGS Debug Release RelWithDebInfo MinSizeRel)
endif()

if(MSVC)
  set(CMAKE_CXX_FLAGS_RELEASE "/O2")
  add_compile_options(/W4)
else()
  set(CMAKE_CXX_FLAGS_RELEASE "-O2")
  add_compile_options(-Wall -Wextra)
endif()

add_library(engine STATIC src/order_book.cpp)
target_include_directories(engine PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/include)

add_executable(trade_engine src/main.cpp)
target_link_libraries(trade_engine PRIVATE engine)

enable_testing()
add_executable(engine_tests tests/test_order_book.cpp)
target_link_libraries(engine_tests PRIVATE engine)
add_test(NAME engine_tests COMMAND engine_tests)
