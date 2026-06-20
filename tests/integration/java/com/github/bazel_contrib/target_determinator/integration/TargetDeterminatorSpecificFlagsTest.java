package com.github.bazel_contrib.target_determinator.integration;

import com.github.bazel_contrib.target_determinator.label.Label;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Set;
import java.util.stream.Collectors;
import java.util.stream.Stream;

import org.eclipse.jgit.api.Git;
import org.eclipse.jgit.util.FileUtils;
import org.hamcrest.CoreMatchers;
import org.junit.After;
import org.junit.Before;
import org.junit.BeforeClass;
import org.junit.Test;

import static junit.framework.TestCase.fail;
import static org.hamcrest.MatcherAssert.assertThat;
import static org.hamcrest.Matchers.containsString;
import static org.hamcrest.Matchers.equalTo;

public class TargetDeterminatorSpecificFlagsTest {
  private static TestdataRepo testdataRepo;

  // Contains a new clone of the testdata repository each time a test is run.
  private static Path testDir;

  @BeforeClass
  public static void cloneRepo() throws Exception {
    testdataRepo = Util.cloneTestdataRepo();
    testDir = Files.createTempDirectory("target-determinator-testdata_dir-clone");
  }

  @Before
  public void createTestRepository() throws Exception {
    testdataRepo.cloneTo(testDir);
  }

  @After
  public void cleanupTestRepository() throws Exception {
    FileUtils.delete(testDir.toFile(), FileUtils.RECURSIVE | FileUtils.SKIP_MISSING);
  }

  @Test
  public void targetPatternFlagAll() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.BAZELRC_TEST_ENV);
    Set<Label> targets =
        getTargets(Commits.TWO_LANGUAGES_OF_TESTS, "//...");
    Util.assertTargetsMatch(
        targets,
        Set.of("//java/example:ExampleTest", "//java/example:OtherExampleTest", "//sh:sh_test"),
        Set.of(),
        false);
  }

  @Test
  public void targetPatternFlagJava() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.BAZELRC_TEST_ENV);
    Set<Label> targets = getTargets(Commits.TWO_LANGUAGES_OF_TESTS, "//java/...");
    Util.assertTargetsMatch(
        targets,
        Set.of("//java/example:ExampleTest", "//java/example:OtherExampleTest"),
        Set.of(),
        false);
  }

  @Test
  public void targetPatternFlagOneTarget() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.BAZELRC_TEST_ENV);
    Set<Label> targets = getTargets(Commits.TWO_LANGUAGES_OF_TESTS, "//java/example:ExampleTest");
    Util.assertTargetsMatch(targets, Set.of("//java/example:ExampleTest"), Set.of(), false);
  }

  @Test
  public void targetPatternFlagOneTargetNotAffected() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.TWO_TESTS);
    Set<Label> targets =
        getTargets(
            Commits.TWO_NATIVE_TESTS_BAZEL5_4_0, "//java/example:ExampleTest");
    Util.assertTargetsMatch(targets, Set.of("//java/example:ExampleTest"), Set.of(), false);
  }

  @Test
  public void targetPatternFlagQueryBeforeWasError() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.ONE_TEST);
    Set<Label> targets = getTargets(Commits.NO_TARGETS, "//java/...");
    Util.assertTargetsMatch(targets, Set.of("//java/example:ExampleTest"), Set.of(), false);
  }

  @Test
  public void targetPatternFlagQueryBeforeWasErrorVerbose() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.ONE_TEST);
    String output = getOutput(Commits.NO_TARGETS, "//java/...", false, true, List.of("--verbose"));
    // This isn't great output, and we shouldn't worry about changing its format in the future,
    // but this test is to ensure we return a result indicating "the query before was bad" rather
    // than "this target didn't exist before".
    assertThat(output, equalTo("//java/example:ExampleTest Changes: ErrorInQueryBefore\n"));
  }

  @Test
  public void targetPatternFlagQueryBeforeWasErrorWhenFatal() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.ONE_TEST);
    try {
      String output = getOutput(Commits.NO_TARGETS, "//java/...", false, true, List.of("--before-query-error-behavior=fatal"));
      fail(String.format("Expected exception but got successful output: %s", output));
    } catch (TargetComputationErrorException e) {
      assertThat(e.getStdout(), CoreMatchers.equalTo("Target Determinator invocation Error\n"));
      assertThat(e.getStderr(), containsString("failed to query at revision 'before'"));
    }
  }

  @Test
  public void failForUncleanRepositoryWithEnforceClean() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.HAS_JVM_FLAGS);

    Files.createFile(testDir.resolve("untracked-file"));

    try {
      getTargets(Commits.TWO_TESTS, "//...", true, true);
      fail("Expected target-determinator command to fail but it succeeded");
    } catch (TargetComputationErrorException e) {
      assertThat(e.getStdout(), CoreMatchers.equalTo("Target Determinator invocation Error\n"));
    }
  }

  @Test
  public void ignoresIgnoredFile() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.TWO_TESTS_WITH_GITIGNORE);

    Path ignoredFile = testDir.resolve("ignored-file");
    Files.createFile(ignoredFile);

    Set<Label> targets = getTargets(Commits.ONE_TEST_WITH_GITIGNORE, "//...", true, true);
    Util.assertTargetsMatch(targets, Set.of("//java/example:OtherExampleTest"), Set.of(), false);

    assertThat("expected ignored file to still be present after invocation", ignoredFile.toFile().exists());
  }

  @Test
  public void failsIfChangingCommitsCausesAnIgnoredFileToBecomeUntracked() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.TWO_TESTS_WITH_GITIGNORE);

    Path ignoredFile = testDir.resolve("ignored-file");
    Files.createFile(ignoredFile);

    try {
      getTargets(Commits.ONE_TEST, "//...", true, true);
      fail("Expected target-determinator command to fail but it succeeded");
    } catch (TargetComputationErrorException e) {
      assertThat(e.getStdout(), CoreMatchers.equalTo("Target Determinator invocation Error\n"));
      assertThat(e.getStderr(), containsString("repository was not clean after checking out revision 'before'"));
    }
  }

  @Test
  public void failForUncleanSubmoduleWithEnforceClean() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.SUBMODULE_CHANGE_DIRECTORY);

    Files.createFile(testDir.resolve("demo-submodule-2").resolve("untracked-file"));

    try {
      getTargets(Commits.SUBMODULE_ADD_DEPENDENT_ON_SIMPLE_JAVA_LIBRARY,
          "//...", true, true);
      fail("Expected target-determinator command to fail but it succeeded");
    } catch (TargetComputationErrorException e) {
      assertThat(e.getStdout(), CoreMatchers.equalTo("Target Determinator invocation Error\n"));
    }
  }

  @Test
  public void testWorktreeCreation() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.TWO_TESTS);
    // Make repository unclean so that a worktree gets created.
    Files.createFile(testDir.resolve("untracked-file"));
    getTargets(Commits.SUBMODULE_ADD_DEPENDENT_ON_SIMPLE_JAVA_LIBRARY,
        "//...", false, false);

    Path worktreePath = TargetDeterminator.getWorktreePath(testDir);
    assertThat("Expected cached git worktree to be present", Files.exists(worktreePath.resolve(".git")));

    getTargets(Commits.SUBMODULE_ADD_DEPENDENT_ON_SIMPLE_JAVA_LIBRARY,
        "//...", false, true);
    assertThat("Expected cached git worktree to be absent", !Files.exists(worktreePath.resolve(".git")));

  }

  @Test
  public void changedConfigurationVerbose() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.BAZELRC_AFFECTING_JAVA);
    String output = getOutput(Commits.TWO_LANGUAGES_OF_TESTS, "//java/example:ExampleTest", false, true, List.of("--verbose"));
    // This isn't great output, and we shouldn't worry about changing its format in the future,
    // but this test is to ensure we return a result including a hint as to what changed the
    // configuration.
    assertThat(output, containsString("-source 7 -target 7"));
  }

  @Test
  public void startupOptsIgnoringBazelrc() throws Exception {
    TestdataRepo.gitCheckout(testDir, Commits.BAZELRC_TEST_ENV);
    Set<Label> targets = getTargets(Commits.TWO_LANGUAGES_OF_TESTS, "//...", false, true, List.of("--bazel-startup-opts=--noworkspace_rc"));
    Util.assertTargetsMatch(targets, Set.of(), Set.of(), false);
  }

  @Test
  public void optimizedExecutionMatchesFullHashingBehavior() throws Exception {
    assertOptimizedMatchesFullHashingAfterLocalCommit(
        this::commitModifiedExampleTest,
        "//java/example:ExampleTest",
        Set.of("//java/example:ExampleTest"),
        "Hash prefill fast path disabled: changed files intersect transitive source graph");

    assertOptimizedMatchesFullHashingAfterLocalCommit(
        this::commitUnusedFile,
        "//java/example:ExampleTest",
        Set.of(),
        "Skipping hash prefill: cquery metadata is unchanged and changed files are outside the transitive source graph");
  }

  private Set<Label> getTargets(String commitBefore, String targets) throws Exception {
    return getTargets(commitBefore, targets, false, true);
  }

  private Set<Label> getTargets(String commitBefore, String targets, boolean enforceClean, boolean deleteCachedWorktree)
      throws Exception {
    return getTargets(commitBefore, targets, enforceClean, deleteCachedWorktree, new ArrayList<>());
  }

  private Set<Label> getTargets(String commitBefore, String targets, boolean enforceClean, boolean deleteCachedWorktree, List<String> flags)
      throws Exception {
    return TargetDeterminator.parseLabels(getOutput(commitBefore, targets, enforceClean, deleteCachedWorktree, flags));
  }

  private String getOutput(String commitBefore, String targets, boolean enforceClean, boolean deleteCachedWorktree, List<String> flags) throws Exception {
    return getResult(testDir, commitBefore, targets, enforceClean, deleteCachedWorktree, flags).stdout();
  }

  private TargetDeterminator.Result getResult(
      Path workspace,
      String commitBefore,
      String targets,
      boolean enforceClean,
      boolean deleteCachedWorktree,
      List<String> flags) throws Exception {
    final List<String> args = Stream.concat(
        Stream.of("--working-directory",
            workspace.toString(),
            "--bazel", "bazelisk",
            "--targets", targets,
            "--nocache_results"
        ),
        flags.stream()
    ).collect(Collectors.toList());
    if (enforceClean) {
      args.add("--enforce-clean=enforce-clean");
    }
    if (deleteCachedWorktree) {
      args.add("--delete-cached-worktree");
    }
    args.add(commitBefore);
    return TargetDeterminator.getResult(workspace, args.toArray(new String[0]));
  }

  private void assertOptimizedMatchesFullHashingAfterLocalCommit(
      WorkspaceChange workspaceChange,
      String targets,
      Set<String> expectedTargets,
      String optimizedPathLog) throws Exception {
    Path optimizedDir = Files.createTempDirectory("target-determinator-optimized-parity");
    Path fullHashDir = Files.createTempDirectory("target-determinator-full-hash-parity");
    try {
      testdataRepo.cloneTo(optimizedDir);
      testdataRepo.cloneTo(fullHashDir);
      TestdataRepo.gitCheckout(optimizedDir, Commits.ONE_TEST_BAZEL7_0_0);
      TestdataRepo.gitCheckout(fullHashDir, Commits.ONE_TEST_BAZEL7_0_0);
      workspaceChange.apply(optimizedDir);
      workspaceChange.apply(fullHashDir);
      Files.createFile(fullHashDir.resolve("untracked-file"));

      TargetDeterminator.Result optimized =
          getResult(optimizedDir, Commits.ONE_TEST_BAZEL7_0_0, targets, false, true, List.of());
      TargetDeterminator.Result fullHash =
          getResult(fullHashDir, Commits.ONE_TEST_BAZEL7_0_0, targets, false, true, List.of());

      assertResultParity(optimized, fullHash, expectedTargets);
      assertThat(optimized.stderr(), containsString(optimizedPathLog));
      assertThat(
          fullHash.stderr(),
          containsString("Source file hash reuse disabled: failed to determine changed files"));
    } finally {
      FileUtils.delete(optimizedDir.toFile(), FileUtils.RECURSIVE | FileUtils.SKIP_MISSING);
      FileUtils.delete(fullHashDir.toFile(), FileUtils.RECURSIVE | FileUtils.SKIP_MISSING);
    }
  }

  private void assertResultParity(
      TargetDeterminator.Result optimized,
      TargetDeterminator.Result fullHash,
      Set<String> expectedTargets) throws Exception {
    Set<Label> optimizedTargets = TargetDeterminator.parseLabels(optimized.stdout());
    Set<Label> fullHashTargets = TargetDeterminator.parseLabels(fullHash.stdout());
    assertThat(optimizedTargets, equalTo(fullHashTargets));
    Util.assertTargetsMatch(optimizedTargets, expectedTargets, Set.of(), false);
  }

  private void commitUnusedFile(Path workspace) throws Exception {
    Path unusedFile = workspace.resolve("unused-file-outside-bazel-graph.txt");
    Files.writeString(unusedFile, "not referenced by any target\n");
    commitPath(workspace, unusedFile.getFileName().toString(), "Add unused file outside Bazel graph");
  }

  private void commitModifiedExampleTest(Path workspace) throws Exception {
    Path sourceFile = workspace.resolve("java/example/ExampleTest.java");
    Files.writeString(sourceFile, Files.readString(sourceFile) + "\n// target-determinator parity test\n");
    commitPath(workspace, "java/example/ExampleTest.java", "Modify example test source");
  }

  private void commitPath(Path workspace, String filePattern, String message) throws Exception {
    try (Git git = Git.open(workspace.toFile())) {
      git.add().addFilepattern(filePattern).call();
      git.commit()
          .setAuthor("Target Determinator Test", "target-determinator@example.invalid")
          .setCommitter("Target Determinator Test", "target-determinator@example.invalid")
          .setMessage(message)
          .call();
    }
  }

  private interface WorkspaceChange {
    void apply(Path workspace) throws Exception;
  }
}
