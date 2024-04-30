# Kubernetes API Versions

在 Kubernetes (k8s) 中，API 版本用于区分不同的 Kubernetes API 版本级别和功能集。在这个目录看到定义：staging/src/k8s.io/api/testdata/v1.28.0，这些 API 版本标识在资源定义中以 `apiVersion` 字段显示，并且根据功能稳定性和支持级别分类为不同的群组。以下是 Kubernetes 中一些常见的 API 版本：
## group和apiVersion的关系
apiVersion 字段在 Kubernetes 资源定义中指定了所使用的 API 的版本。这个字段由两部分组成：group/version。例如，apps/v1 中，apps 是 API 组，而 v1 是该组内的版本号。如果资源属于 Kubernetes 的核心组（不属于任何具体的组），apiVersion 将只显示版本号，如 v1。
## 目录关系
- 1、在pkg/apis下面，addKnownTypes可以知道有哪些类型
- 2、staging/src/k8s.io/api/xx/xx/register.go下面也有具体的类型
## Core Group (无组前缀)

- **`v1`**：这是最常见的 API 版本，包括许多核心组件，如 Pods、Services、ConfigMaps 和 Namespaces。

## Named Groups

这些 API 版本带有特定的组名前缀，并且通常表示 Kubernetes 功能的特定区域。

### Apps Group (`apps`)

- **`apps/v1`**：包括支持应用部署的资源，如 Deployments、ReplicaSets 和 StatefulSets。

### Extensions Group (`extensions`)

- **`extensions/v1beta1`**（已弃用）：最初用于测试新的核心功能，如 Ingress。许多功能已经迁移到其他更稳定的 API 组。

### Batch Group (`batch`)

- **`batch/v1`**：包括 Job 和 CronJob。
- **`batch/v1beta1`**：包括定时任务的较早版本。

### Networking Group (`networking.k8s.io`)

- **`networking.k8s.io/v1`**：包括 Ingress 和 NetworkPolicy。

### RBAC Authorization Group (`rbac.authorization.k8s.io`)

- **`rbac.authorization.k8s.io/v1`**：包括角色和角色绑定。

### Storage Group (`storage.k8s.io`)

- **`storage.k8s.io/v1`**：包括存储相关的资源，如 StorageClasses 和 VolumeAttachments。

### API Extensions Group (`apiextensions.k8s.io`)

- **`apiextensions.k8s.io/v1`**：用于创建自定义资源（CRDs）。

## Alpha 和 Beta 版本

Kubernetes 还经常发布 alpha (`v1alpha1`) 和 beta (`v1beta1`, `v1beta2`) 版本的 API，这些 API 提供对即将推出功能的早期访问，但不推荐在生产环境中使用，因为它们可能会更改或删除。

## 如何查找可用的 API 版本

你可以使用 `kubectl` 命令来查看集群中可用的所有 API 版本和资源。这可以通过以下命令完成：

```bash
kubectl api-versions
