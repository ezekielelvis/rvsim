cmake_minimum_required(VERSION 3.24)
project(rvcore LANGUAGES C CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
endif()

option(RVSIM_SANITIZE "Build with AddressSanitizer + UndefinedBehaviorSanitizer" OFF)
option(RVSIM_BUILD_TESTS "Build GoogleTest unit tests" ON)

set(RVSIM_WARNINGS -Wall -Wextra -Wpedantic -Werror -Wshadow -Wconversion -Wsign-conversion)

if(RVSIM_SANITIZE)
  set(RVSIM_SAN_FLAGS -fsanitize=address,undefined -fno-sanitize-recover=all -fno-omit-frame-pointer)
  add_compile_options(${RVSIM_SAN_FLAGS})
  add_link_options(${RVSIM_SAN_FLAGS})
endif()

# The core library linked into the Go binary through cgo. Built without
# exceptions or RTTI so nothing can unwind across the C ABI.
add_library(rvcore STATIC
  src/abi.cpp
  src/clint/clint.cpp
  src/cpu/execute.cpp
  src/csr/csr.cpp
  src/decode/decode.cpp
  src/mmu/mmu.cpp
  src/state/state.cpp
)
target_include_directories(rvcore
  PUBLIC include
  PRIVATE src
)
# Vendored xxHash v0.8.3 (BSD-2-Clause), outside our warning flags.
target_include_directories(rvcore SYSTEM PRIVATE third_party/xxhash)
target_compile_options(rvcore PRIVATE ${RVSIM_WARNINGS} -fno-exceptions -fno-rtti)

# Tell Go when the library changes (see cmake/write_corehash.cmake).
add_custom_command(TARGET rvcore POST_BUILD
  COMMAND ${CMAKE_COMMAND}
    -DLIB=$<TARGET_FILE:rvcore>
    -DOUT=${CMAKE_CURRENT_SOURCE_DIR}/../vmm/machine/zz_corehash.go
    -P ${CMAKE_CURRENT_SOURCE_DIR}/cmake/write_corehash.cmake
  VERBATIM)

if(RVSIM_BUILD_TESTS)
  include(FetchContent)
  FetchContent_Declare(googletest
    GIT_REPOSITORY https://github.com/google/googletest.git
    GIT_TAG v1.18.0
    GIT_SHALLOW TRUE
  )
  set(INSTALL_GTEST OFF CACHE BOOL "" FORCE)
  set(BUILD_GMOCK OFF CACHE BOOL "" FORCE)
  FetchContent_MakeAvailable(googletest)

  enable_testing()
  add_executable(rvcore_tests
    tests/abi_test.cpp
    tests/alu_test.cpp
    tests/atomic_test.cpp
    tests/clint_test.cpp
    tests/compressed_test.cpp
    tests/control_test.cpp
    tests/decode_test.cpp
    tests/header_c_test.c
    tests/memory_test.cpp
    tests/mmu_test.cpp
    tests/state_test.cpp
    tests/breakpoint_test.cpp
    tests/smc_test.cpp
    tests/mmio_test.cpp
    tests/muldiv_test.cpp
    tests/privileged_test.cpp
    tests/system_test.cpp
  )
  target_include_directories(rvcore_tests PRIVATE src)
  set_source_files_properties(tests/header_c_test.c PROPERTIES COMPILE_OPTIONS "-std=c11;-Wall;-Wextra;-Werror")
  target_link_libraries(rvcore_tests PRIVATE rvcore GTest::gtest_main)
  target_compile_options(rvcore_tests PRIVATE $<$<COMPILE_LANGUAGE:CXX>:${RVSIM_WARNINGS}>)
  include(GoogleTest)
  gtest_discover_tests(rvcore_tests)
endif()
