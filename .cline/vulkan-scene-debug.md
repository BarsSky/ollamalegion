# Cline Skill: Vulkan Scene Not Rendering — Descriptor/Debug Workflow

**When to use**
- Vulkan test application (`test_glfw`, `test_gltf_animate`, etc.) starts but scenes/models/skybox are invisible.
- Validation layer logs contain descriptor-set layout mismatch errors or “secondary command buffer has no commands” errors.
- Working in `pk_display` MinGW/CMake build.

---

## Step 1 — Build and run the failing test with validation layers

```powershell
cd build\windows-debug-mingw
cmake --build . --target pm_io_vulkan test_glfw -- -j4
copy lib\pm_io_vulkan\libpm_io_vulkan.dll tests\test_glfw\
cd tests\test_glfw

powershell -Command "$wd = (Get-Location).Path; Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $wd 'stderr_visible.log'), (Join-Path $wd 'stdout_visible.log'); $proc = Start-Process -FilePath (Join-Path $wd 'test_glfw.exe') -WorkingDirectory $wd -RedirectStandardOutput (Join-Path $wd 'stdout_visible.log') -RedirectStandardError (Join-Path $wd 'stderr_visible.log') -PassThru; Start-Sleep -Seconds 15; if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force } else { Write-Host 'Exited early' $proc.ExitCode }"
```

Check:
- `stderr_visible.log` for validation errors.
- `stdout_visible.log` for repeated `endRender`/`submitFrame` calls (proves frames are being submitted).

---

## Step 2 — Classify the validation error

Common patterns:

| Error | Likely cause |
|-------|--------------|
| `vkCmdBindDescriptorSets(): ... not compatible with ... pipeline layout` binding type mismatch (e.g. UBO vs storage buffer) | `Object::drawObjectBase()` is binding the base pick descriptor to a pipeline that does not expect it. |
| `... has N total descriptors, but ... trying to bind, has M total descriptors` | Pipeline layout set N has a different descriptor count than the bound descriptor set. |
| `vkCmdExecuteCommands(): ... has no commands` / `vkQueueSubmit(): ... unrecorded command buffer` | A secondary command buffer handle was added to an execute list but never `vkBeginCommandBuffer`/recorded/`vkEndCommandBuffer`. |

---

## Step 3 — Inspect the draw and layout pipeline

Files to open:

- `lib/pm_io_vulkan/include/pk_display/vk_display_object.hpp`
  - Confirm `drawObjectBase(VkCommandBuffer)` is `virtual`.
  - Confirm `Object::object_draw()` calls `drawObjectBase(_buffer); draw(_buffer);`.

- `lib/pm_io_vulkan/vk_display_object.cpp`
  - `Object::setObjectBaseLayout()` — base pick layout: set 0 = storage buffer (binding 0) + UBO (binding 1).
  - `Object::drawObjectBase()` — binds `pickDescriptor[ImageIndex]` at set 0.

- `lib/pm_io_vulkan/Models/type_*.cpp`
  - `setDescriptorLayout()` — what sets/bindings are declared.
  - `draw()` — which descriptor sets are bound at which `firstSet`.

- `lib/pm_io_vulkan/vk_display_impl.cpp`
  - `generateStaticObjectsCommandBuffer()` — are secondary buffers actually recorded via thread jobs?
  - `threadRenderFunction*` helpers.

- `lib/pm_io_vulkan/gltfmodel.cpp` / `gltf_model_impl.h`
  - vkglTF descriptor layouts: how many bindings in the UBO set and image set?
  - `Model::drawNode()` fixed set indices: UBO at set 1, images at `bindImageSet + 1`.

---

## Step 4 — Apply standard fixes

### 4a. GLTF/SkyBox must skip the base pick draw

If an object uses its own 1- or 3-set pipeline layout and never uses the base picking SSBO, override `drawObjectBase()` as a no-op:

```cpp
// in the derived class header
void drawObjectBase(VkCommandBuffer) override {}
```

Applies to: `GLTF_Model`, `GLTF_SkyBox`, standalone `SkyBox`.

### 4b. GLTF pipeline layout must match vkglTF set indices

GLTF layout should normally be:

- set 0: UBO (binding 0, VERTEX) — optionally also sampler if skybox variant
- set 1: UBO (binding 0, VERTEX) — matches `axis.vert`/`shader.vert`
- set 2: sampler(s) — matches `axis.frag` and vkglTF image set

Do **not** include `Object::pickDescriptorSetLayout` in the GLTF pipeline layout. Keep base-pick ownership in `Object::pickDescriptorSetLayout` only.

### 4c. Image descriptor set must match vkglTF normal-map usage

`GLTF_Model` constructor calls `u_ptr_model->enable_normalMap()`. Therefore the vkglTF image descriptor set layout has two bindings: base color (0) and normal map (1). The pipeline layout set 2 must declare **both** bindings, not one:

```cpp
std::array<VkDescriptorSetLayoutBinding, 2> samplerBindings{};
samplerBindings[0] = {0, VK_DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER, 1, VK_SHADER_STAGE_FRAGMENT_BIT, nullptr};
samplerBindings[1] = {1, VK_DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER, 1, VK_SHADER_STAGE_FRAGMENT_BIT, nullptr};
```

Apply the same layout in both `GLTF_Model::setDescriptorLayout()` and `GLTF_SkyBox::setDescriptorLayout()`.

### 4d. Own GLTF layouts without double-free

Store GLTF-specific layouts in a dedicated raii vector:

```cpp
protected:
  std::vector<pk_display::internal::raii::DescriptorSetLayout>
      gltfDescriptorSetLayouts_{};
```

Push raw handles into the non-owning `vkDescriptorLayouts` aggregator, but never call `vkDestroyDescriptorSetLayout` on aggregator entries.

### 4e. Record skybox secondary command buffers

If `skybox_objects` are appended to `lcommandBuffersStatic` but never recorded, add a thread render helper:

```cpp
void VKDisplay::CImpl::threadRenderFunctionSKYBOX(
    uint32_t threadIndex, uint32_t bufferCount,
    VkCommandBufferInheritanceInfo inheritanceInfo)
{
  Object* object = skybox_objects.at(threadIndex);
  VkCommandBuffer cmdBuffer = skybox_objects[threadIndex]->cmdBuffer[bufferCount];

  VkCommandBufferBeginInfo beginInfo = initializers::commandBufferBeginInfo();
  beginInfo.flags = VK_COMMAND_BUFFER_USAGE_RENDER_PASS_CONTINUE_BIT;
  beginInfo.pInheritanceInfo = &inheritanceInfo;
  VK_CHECK_RESULT(vkBeginCommandBuffer(cmdBuffer, &beginInfo));

  // viewport/scissor
  // object->object_draw(cmdBuffer);

  VK_CHECK_RESULT(vkEndCommandBuffer(cmdBuffer));
}
```

And dispatch it inside `generateStaticObjectsCommandBuffer()` before pushing the handle into `lcommandBuffersStatic`.

---

## Step 5 — Rebuild, deploy, retest

```powershell
cd build\windows-debug-mingw
cmake --build . --target pm_io_vulkan test_glfw -- -j4
copy lib\pm_io_vulkan\libpm_io_vulkan.dll tests\test_glfw\
cd tests\test_glfw
# run the PowerShell snippet from Step 1 again
```

Acceptance criteria:
- `stderr_visible.log` has no `vkCmdBindDescriptorSets` compatibility errors.
- `stderr_visible.log` has no “secondary command buffer has no commands” / unrecorded-buffer errors.
- `stdout_visible.log` shows continuous `endRender`/`submitFrame` calls during the run.

---

## Quick grep patterns

```bash
# Find all drawObjectBase overrides and callers
grep -R "drawObjectBase" lib/pm_io_vulkan --include="*.cpp" --include="*.hpp"

# Find descriptor set bindings
grep -R "vkCmdBindDescriptorSets" lib/pm_io_vulkan/Models --include="*.cpp"

# Find secondary buffer recording helpers
grep -R "threadRenderFunction" lib/pm_io_vulkan/vk_display_impl.cpp

# Find vkglTF image layout creation
grep -n "descriptorSetLayoutImage" lib/pm_io_vulkan/gltfmodel.cpp